# BitBlocker

> **Back to:** [[BitSalt-Projects]]
> **Started:** 2026-04-21
> **Status:** 🟢 v1.0.0 released (2026-07-20) — v1.1 observability in planning
> **One-liner:** Self-hosted Go daemon that silently drops inbound traffic from specified countries at the Traefik edge, for operators running small-scale self-hosted infrastructure.

**Sprint cadence:** 2 weeks, project-own cadence (independent of the platform's 7-day standard; see Sprint 2 note 2026-05-10 re: platform Sprint 3 window overlap). See the Cadence-drift note under CURRENT below — this cadence is not tracking real elapsed time and is flagged, not yet resolved.

---

## Overview

BitBlocker is a single-binary Go daemon that blocks inbound scanning traffic from selected countries before it reaches application code. It sits behind Traefik as a `forwardAuth` middleware, maintains an in-memory CIDR trie populated from DB-IP "IP-to-Country Lite" data (ADR 0003, 2026-07-05 — supersedes the original MaxMind GeoLite2 choice), and refreshes itself on a cron schedule.

v1.0 (released 2026-07-20) ships country-based blocking with IPv4+IPv6 support and a fail-closed-by-default security posture, plus an opt-in `behavior.startup_mode: fail-open` path gated on a readiness contract with a mandatory heartbeat (ADR 0004).

v1.1 adds Prometheus metrics and CLI inspection tools. ASN-level blocking via BGP.tools is deferred until the data-source access question is resolved.

---

## Milestones

| Milestone | Target sprint | Status |
|---|---|---|
| Core engine (CIDR trie + MMDB reader + `atomic.Pointer` swap) | Sprint 2 | ✅ |
| Daemon serves `/check` + `/healthz` with fail-closed cold start | Sprint 3 | ✅ |
| v1.0 released (binaries + Docker image + operator docs) | Sprint 4 | ✅ |
| v1.1 observability released (metrics + CLI) | Sprint 5 | ⬜ |

---

## CURRENT — Sprint 5 (v1.1 observability)

**Sprint goal:** v1.1 shipped with Prometheus metrics and CLI inspection tooling — on top of a test seam that protects the new config surface.

**Sprint 4 closed 2026-07-20 — v1.0.0 released.** Every Sprint 4 task shipped; full task-by-task detail is preserved in the Archive below. The release was verified anonymously after tagging: the GitHub release carries linux/amd64 + linux/arm64 binaries with checksums, and the ghcr package publishes tags `1.0.0`, `1.0`, `1`, and `latest`, with the `:latest` manifest returning HTTP 200 for both architectures. Two release rehearsals (`v1.0.0-rc1`, `v1.0.0-rc2`) preceded the real tag and each caught a defect that would otherwise have shipped — see the Decisions Log below. **Sprint 5 (v1.1 observability) is now the active sprint.**

**Cadence-drift note (still flagged, still not resolved).** Sprint 4's stated calendar window was Jun 2 – Jun 15; it closed 2026-07-20, five weeks past. This is the same drift flagged at Sprint 3 close and it has now recurred. Sprint dates on this project are not tracking real elapsed time — plausibly reflecting its solo/part-time pacing rather than the 2-week planning window. Surfaced per PM Standards ("cadence is observed, not imposed"); not resolved here — a Jeff-level cadence call. Carried below. Sprint 5 is deliberately opened **without** a stated calendar window rather than with one that will be wrong on arrival.

### Tasks

**Sequencing is load-bearing:** the `fetcher.BaseURL` test seam (OQ-10) comes **first**, ahead of all metrics and CLI work. Sprint 5 adds new config fields (the metrics admin listener, the refresh-failure webhook URL), and the missing seam means `cmd/bitblocker/main.go` wiring is untestable — new config fields landed before the seam exists would ship unprotected. Do not reorder.

| Task | Status | Notes |
|---|---|---|
| Inject `fetcher.BaseURL` to make `main.go` wiring testable (OQ-10) | ⬜ | **FIRST — blocks every task below.** Decision 543: OQ-10 was deferred out of v1.0 but sequenced ahead of the metrics work precisely because the metrics/webhook config fields would otherwise land without a test seam covering their wiring |
| `/metrics` Prometheus endpoint on separate admin listener | ⬜ | Adds a new config field — depends on the OQ-10 seam |
| Alert webhook on refresh failure | ⬜ | Adds a new config field (webhook URL) — depends on the OQ-10 seam |
| `bitblocker check <ip>` CLI subcommand | ⬜ | |
| `bitblocker list` CLI subcommand | ⬜ | |
| Live heartbeat-ticker test coverage (OQ-FAILOPEN-5) | ⬜ | Test gap deferred out of v1.0 with the ADR 0004 fail-open work; the mandatory heartbeat's live ticker is not exercised under test |
| Tag v1.1 and publish release | ⬜ | Rehearse with an `rc` tag first — the practice earned its keep twice at v1.0 (see Decisions Log) |

### Open Questions

> **Retired from this file (ADR 0041, accepted 2026-06-18).** OQ state and narrative are both DB-canonical as of this pass (2026-07-06 — bitblocker's first migration to the interim sprint-file format). This file previously carried a live Open Questions table (through Sprint 3); it is now frozen as historical record in the Archive below.
>
> - **OQ state** (open / closed / resolved, ownership, subject label): query `get_open_questions(slug="bitblocker")`.
> - **OQ narrative** (question text + resolution rationale): the Archive's frozen table below carries the narrative as of freeze time. Going forward, new OQs are created via `create_open_question` (orchestrator-emitted on PM's behalf) and narrative lives in this CURRENT block or the Decisions Log below.
> - Git history retains this file's pre-freeze OQ table for audit purposes.
>
> **Status snapshot (2026-07-20, 18 OQs total).** OPEN (7) — OQ-2 (ASN blocking scope), OQ-3 (allowlist), OQ-4 (leftmost-XFF knob), OQ-6 (optional `maxminddb` v2 adoption), OQ-10 (`main.go` wiring untestable — v1.1, sequenced FIRST), OQ-FAILOPEN-3 (the declared-but-unconsumed `startup_mode` knob; see the dated correction in the Archive's Sprint 3 table), OQ-FAILOPEN-5 (live heartbeat-ticker test gap — v1.1). RESOLVED (10) — OQ-1, OQ-5, OQ-7, OQ-8, OQ-9, OQ-CACHE-2, OQ-CACHE-3, OQ-FAILOPEN-1, OQ-FAILOPEN-2, OQ-FAILOPEN-4. CLOSED (1) — OQ-CACHE-1. Query `get_open_questions(slug="bitblocker")` for live state; this snapshot is a convenience copy and is not authoritative.

### Decisions Log (current sprint)

| Date | Decision | Reasoning |
|---|---|---|
| 2026-07-20 | **v1.0.0 released** (DB decision 546). Tagged and published after two rehearsals; verified anonymously — full release with linux/amd64 + linux/arm64 binaries and checksums, ghcr tags `1.0.0` / `1.0` / `1` / `latest`, `:latest` manifest HTTP 200 across both platforms. | The two release rehearsals (`v1.0.0-rc1`, `v1.0.0-rc2`) each caught a defect that would otherwise have shipped in the real tag: (1) the Dockerfile builder was pinned to golang 1.25.11 while `go.mod` declares 1.25.12 — a hard failure under `GOTOOLCHAIN=local`, invisible to normal CI because CI does not build the release image the same way; (2) the ghcr package published **private** by default, so the image would have been unpullable by exactly the self-hosted audience the project targets. Neither was a code defect — both lived in release plumbing that only a full dry run exercises. This is the argument for rehearsing every future tag rather than treating the rehearsal as v1.0 ceremony (PR #24 also added a build-drift CI guard so the Dockerfile/`go.mod` toolchain skew cannot silently recur). |
| 2026-07-20 | **v1.0 tag HELD for the ADR 0004 fail-open work**; ADR 0004 promoted proposed→accepted (DB decision 539). Landed via PR #21 (ADR), PR #22 (implementation), PR #23 (docs). | The `behavior.startup_mode` knob had been declared in config since Sprint 1 but was never consumed by any code path (see the dated correction in the Archive's Sprint 3 task table). Shipping v1.0 with a documented config knob that silently does nothing would have been a correctness and trust defect in an authorization component — worse than shipping late. The accepted design wires fail-open at the **readiness gate** with a **mandatory heartbeat**, so a daemon in fail-open cannot sit silently degraded: the absence of the heartbeat is itself the stuck-state signal an operator can alert on. |
| 2026-07-20 | **The bitblocker repo stays PUBLIC** (DB decision 540) — it is the only public repo in the org. Residual deferred to deploy time: header-trust configuration. | Verified before deciding rather than assumed: no secrets in the working tree or in git history, and the daemon is not deployed anywhere in the estate. The project is also positioned as noise reduction at the edge, **not** an authentication boundary — so public source does not hand an attacker a bypass for a control anyone is relying on for security. The one genuinely deployment-sensitive surface is header trust (`X-Real-IP` / XFF handling depends on what the fronting proxy is configured to do), and that is a per-deployment configuration concern, not a reason to close the source. |
| 2026-07-20 | **OQ-10 deferred out of v1.0 but sequenced FIRST in v1.1**, ahead of the Sprint 5 metrics work (DB decision 543). | `cmd/bitblocker/main.go`'s wiring is untestable because `fetcher.BaseURL` is not injectable. Deferring the fix past v1.0 was the right call — it is a test-seam gap, not a shipped-behavior defect. But it cannot be deferred *within* v1.1: Sprint 5 introduces new config fields (the metrics admin listener, the refresh-failure webhook URL), and every one of them is wired through exactly the `main.go` path the missing seam leaves uncovered. Fixing the seam after the fields land means the fields ship unprotected and the seam work then has to retrofit coverage onto code already in a release. Order matters more than priority here. |
| 2026-07-19 | **OQ-CACHE-2 resolved: on detecting a corrupt or stale cache file at startup, remove it** — non-fatally (DB decision 522). PR #18. | Matches the Architect lean recorded when the OQ was filed. Leaving the unusable file in place means every subsequent start re-trips the same failed load and re-emits the same WARN, so the daemon accumulates a permanent noisy-but-harmless error that operators learn to ignore — the worst outcome for a signal that should mean something. Removal is safe because the cache is a pure derived artifact (the raw MMDB), regenerable by the next fetch. The removal is non-fatal for the same reason cache-write failure is non-fatal: the cache is an optimization on the cold-start path, never a correctness dependency. |
| 2026-07-06 | CI toolchain fix: `golangci-lint-action` v6→v7 (resolves OQ-8); `go` directive bumped 1.22.2→1.25.11, closing all `govulncheck` stdlib findings (scan = 0 vulns; moots OQ-7). `maxminddb-golang` stays pinned at v1.13.1 — the Go 1.24 floor only matters if/when `v2` is adopted (OQ-6, now purely optional and decoupled from security). PR #15. | `golangci-lint` v2.11.4 requires action `@v7`; a pre-existing gap surfaced 2026-05-08 (OQ-8). The toolchain bump was the cheapest way to retire all eight stdlib `govulncheck` findings at once (OQ-7) rather than triage per-CVE. |
| 2026-07-05 | GeoIP country source switched from MaxMind GeoLite2-Country to DB-IP "IP-to-Country Lite" (ADR 0003). **Amends** the 2026-04-22 decision "MaxMind consumed as MMDB binary format (not CSV)" — the *provider* changes; the MMDB-binary-not-CSV half is retained unchanged. No account/key/cost; public dated MMDB download URL; CC-BY 4.0 attribution (README + `NOTICE`, lands with the Sprint 4 README task). Drop-in for `internal/mmdb/loader.go` (decodes only `country.iso_code`, which DB-IP also carries) — zero loader change. Dissolves OQ-1 (MaxMind license-key procurement) entirely. PR #13. | MaxMind free-tier license-key procurement was blocking the Sprint 3 fetcher end-to-end, and MaxMind was non-responsive / steering toward paid plans — friction the self-hosted hobbyist audience shouldn't have to absorb. The spec's own pre-v1 Open Question had already flagged DB-IP as exactly this alternative. See ADR 0003 for the full source comparison. |

### Carry-over Log (current sprint)

| Task | Original sprint | Moved to | Reason |
|---|---|---|---|
| Sprint calendar drift (see Cadence-drift note above) | Sprint 3 | Sprint 5 (still flagged, not resolved) | Bitblocker's stated sprint windows are not tracking real elapsed time — now recurred at Sprint 4 close (stated Jun 2–15, closed 2026-07-20). Not resolving unilaterally — a Jeff-level cadence call. Trigger: next SPRINT-REVIEW or a Jeff-directed cadence conversation. |
| OQ-10 — inject `fetcher.BaseURL` to make `main.go` wiring testable | Sprint 4 (deferred) | Sprint 5, task 1 | Deferred out of v1.0 (test-seam gap, not shipped-behavior defect). Sequenced FIRST in v1.1 per decision 543 — the metrics/webhook config fields wire through the uncovered `main.go` path. Now a Sprint 5 task, not a floating carry-over. |
| OQ-FAILOPEN-5 — live heartbeat-ticker test coverage | Sprint 4 (deferred) | Sprint 5 | Test gap deferred out of v1.0 alongside the ADR 0004 fail-open work; the mandatory heartbeat's live ticker is not exercised under test. Now a Sprint 5 task. |
| OQ-2 / OQ-3 / OQ-4 / OQ-6 — open feature/scope questions | Sprint 3 and earlier | Standing open | ASN blocking scope (OQ-2), allowlist (OQ-3), leftmost-XFF knob (OQ-4), optional `maxminddb` v2 adoption (OQ-6). None gate v1.1; each is a Jeff/roadmap scope call revisited when its feature is scheduled. |

**Last updated:** 2026-07-20 — PM Sprint 4 close-out / Sprint 5 open: v1.0.0 released (PRs #18–#24; tags rc1/rc2/1.0.0); ADR 0004 fail-open wired; OQ-FAILOPEN-3 false-completion correction applied to the Archive; Sprint 5 opened with OQ-10 sequenced first.

> _Sprint-file format: interim CURRENT/Archive split per the `sprint-file-format` skill (this pass is bitblocker's first migration to the format). The Open Questions table is retired per ADR 0041 — see pointer note above._

---

## Archive — frozen 2026-07-06

_This section holds all pre-2026-07-06 sprint file content, migrated from the prior flat format in this pass. Frozen: do not append to it or edit it further, except the one-time reconciliation applied at this freeze (Sprint 3 tasks marked ✅ to reflect PRs #13–#15; the historical OQ table's OQ-1/5/7/8 marked resolved and OQ-6 reworded — all state already true as of 2026-07-05/06 but not yet landed in this file at freeze time)._

### Sprint 1 — Apr 21 to May 4

**Goal:** Repo scaffolded and CIDR trie (IPv4+IPv6) passes unit tests.

| Task | Status | Notes |
|---|---|---|
| Author Go coding-standards addendum at `~/projects/coding-standards/coding-standards-go.md` | ✅ | Pre-Sprint-1 — blocks all coding tasks. Also add reference to `~/.claude/CLAUDE.md` addenda list |
| Initialize Go module and repo structure (`cmd/`, `internal/…`) per spec | ✅ | Module `github.com/bitsalt/bitblocker`; `cmd/bitblocker` + `internal/{blocklist,fetcher,server,config}` stubs, Makefile, `.gitignore`. `make` not yet installed locally — run `go build`/`go test` directly until then |
| GitHub Actions CI skeleton (build + test on push) | ✅ | `.github/workflows/ci.yml`: build, vet, race-enabled tests, `go mod verify`, `govulncheck`. `golangci-lint` deferred — needs `.golangci.yml` (not yet in sprint plan, see Carry-over) |
| Config schema (YAML) with validation | ✅ | `internal/config`: typed structs, `Load`/`Validate`, `MAXMIND_LICENSE_KEY` env override, `behavior.startup_mode` knob, `config.example.yaml`. Full cron-expression validation deferred to Sprint 3 scheduler task (avoids pulling `robfig/cron/v3` before it's used). **[`MAXMIND_LICENSE_KEY` removed 2026-07-05 per ADR 0003 — see Sprint 3 below; this row is historical record of what Sprint 1 actually shipped.]** |
| Structured JSON logging setup | ✅ | `internal/logging`: `log/slog` JSON/text handlers selected from `config.LoggingConfig`, `WithContext`/`FromContext` propagation, discard-logger fallback (no `slog.Default()` reads), `Redact()` with stable 4-byte SHA-256 prefix. Not yet wired into `main` — lands with the HTTP server in Sprint 2 |
| CIDR trie supporting IPv4 + IPv6 lookups | ✅ | Bit-level radix trie, separate v4/v6 roots, built on `net/netip`. Insert masks host bits, is idempotent, ignores invalid/mismatched-family prefixes. Contains normalizes IPv4-in-IPv6 via `Unmap`. Benchmarks: ~39 ns/op IPv4, ~211 ns/op IPv6 against 10k-prefix set — comfortably under the spec's 1ms budget |
| Unit tests for trie (insert, lookup, edge cases) | ✅ | Merged with the trie task per TDD — table-driven coverage in `trie_test.go` for single-host, nested, disjoint, dual-stack normalization, idempotency, invalid-input, and mixed-family `Len()` |

---

### Sprint 2 — May 5 to May 18

**Goal:** Daemon answers `/check` and `/healthz` correctly against a preloaded MMDB fixture.

**Sprint 2 mid-sprint status (2026-05-10 portfolio sprint review).** Request-path slice closed via PR #5 (merge `20b096f`, 2026-05-08): `/check`, `X-Real-IP`/rightmost-XFF extraction, `/healthz`, fail-closed posture, handler tests all ✅. Sprint goal **partially met** — daemon answers `/check` / `/healthz` correctly against an empty `LookupSource` (always-allow stub); the swap+disk-cache slice (atomic swap mechanism + disk cache snapshot read/write) carries through the second half of bitblocker's own 2-week Sprint 2 cadence (May 5 → May 18) and crosses into the platform Sprint 3 calendar window (Mon 2026-05-11 → Sun 2026-05-17). **Per OQ-PORTFOLIO-19 closure 2026-05-10 (portfolio sprint review):** bitblocker Sprint 3 allocation ~6 hr/wk (matches the standing 4–6 hr time-budget envelope; expanded slightly to fit absent rework debt). The swap+disk-cache slice fits the ~6 hr envelope absent rework debt; the seam is the `Lookup` interface contract, where today's empty `LookupSource` becomes an `atomic.Pointer[blocklist.Trie]`-backed source with no server-side change. **bitblocker's own Sprint 3** (May 19 → Jun 1) is outside the platform Sprint 3 calendar window and is unaffected by this allocation.

**Sprint 2 close (2026-05-15).** Swap+disk-cache slice landed via PR #9 (ADR 0001 + ADR 0002 + interface spec shipped via PR #8; implementation via PR #9; `go test -race ./...`, lint, vuln-scan all clean; `go.mod`/`go.sum` untouched). Sprint 2 goal is now **fully met**: the daemon consults a populated trie via `blocklist.Source` backed by `atomic.Pointer[blocklist.Trie]`, with a disk-cache cold-start fast-path (`internal/diskcache` package). See decisions log 2026-05-15 for mechanism details.

| Task | Status | Notes |
|---|---|---|
| Integrate `maxminddb-golang` reader and populate trie from MMDB | ✅ | `internal/mmdb/{doc,loader,loader_test}.go`. Pinned `maxminddb-golang v1.13.1` and `mmdbwriter v1.0.0` (test-only) — both pinned below the Go 1.24 floor that newer releases introduced, to stay compatible with the project's `go 1.22.2` toolchain. Loader uses `Networks(SkipAliasedNetworks)` and switches on `len(net.IP)` to build `netip.Prefix` in the form `Trie.Insert` expects. Country match is on `country.iso_code` only — see Open Questions for the `registered_country` scope decision. Lesson `lessons/maxminddb/version-floors-and-aliasing-gotchas.md` written in agent-knowledge-base. PR #2 |
| Atomic swap mechanism (lock-free via `atomic.Pointer[blocklist.Trie]`) | ✅ | `internal/blocklist/source.go`: `Source` type with `Current()` / `Swap()`. One `atomic.Pointer.Load()` on the `/check` hot path — no lock, no cacheline write. `RWMutex` explicitly rejected in ADR 0001 (adds `RLock`/`RUnlock` to every `/check` for no benefit; the trie is immutable after construction). Wired in `cmd/bitblocker` via a closure returning untyped `nil` when empty (nil-interface-trap guard). PR #9; ADR 0001 |
| Disk cache: write snapshot on successful load, read on startup | ✅ | `internal/diskcache` package (not `internal/blocklist` — placing it there would have created an import cycle: `internal/mmdb` already imports `internal/blocklist`; `diskcache` imports both). Cache artifact is the raw MMDB file (no bespoke codec); startup read calls `mmdb.LoadCountryBlocklist` verbatim — one code path from artifact to trie. Write is temp-file + `Sync` + atomic rename (POSIX crash-safe); staleness check via `Stat` against `cache.max_age` (default 48h). Corrupt or stale cache logs `WARN` and falls back to fail-closed cold start; cache-write failure is non-fatal (`WARN`). Two new config fields: `cache.path` (default `/var/cache/bitblocker/GeoLite2-Country.mmdb`) and `cache.max_age` (default 48h) — field placement (dedicated `cache:` block vs. under `behavior:`) is OQ-CACHE-1 (Developer implemented `cache:` block in `config.example.yaml`). PR #9; ADR 0002 |
| HTTP server with `/check` endpoint | ✅ | Request-path slice (PR #5; merge `20b096f`; landing commit `55db825` "server(http): land /check + /healthz request-path slice"). `/check` returns 200/403 against the daemon's `Lookup` interface seam; today's wiring uses an empty `LookupSource`, so all `/check` calls return 200 (allow) until the swap+disk-cache slice replaces it with an `atomic.Pointer[blocklist.Trie]`-backed source. Request-path semantics (header selection, fail-closed, healthz) all in place |
| Client IP extraction: `X-Real-IP` first, rightmost-XFF fallback | ✅ | Landed in PR #5 alongside `/check`. `X-Real-IP` consulted first; falls back to rightmost entry of `X-Forwarded-For` per decisions log 2026-04-22. Leftmost-XFF deferred to a future config knob (Open Question) |
| `/healthz` endpoint (returns 503 while blocklist empty) | ✅ | Landed in PR #5. Returns 503 while `LookupSource` is empty (cold-start posture); flips to 200 once the trie is populated. Empty source today is the "always allow" stub; once swap+disk-cache slice lands, `/healthz` actually reflects ruleset state |
| Fail-closed on unparseable `/check` with WARN log | ✅ | Landed in PR #5. Unparseable IP → 403 + WARN-level structured log via `internal/logging`. Aligns with decisions-log 2026-04-22 fail-closed posture |
| HTTP handler tests | ✅ | Landed in PR #5. `httptest`-based table-driven tests exercise: header-selection precedence, malformed input fail-closed, `/healthz` empty/non-empty paths, `/check` allow/deny against a stubbed `Lookup` |

---

### Sprint 3 — May 19 to Jun 1

**Goal:** Daemon fetches, refreshes, and survives cold-start failures safely end-to-end.

**Sprint 3 close (2026-07-06).** All six tasks shipped across three merged PRs: PR #13 (ADR 0003 — GeoIP source switch from MaxMind GeoLite2 to DB-IP "IP-to-Country Lite," dissolving the MaxMind-license-key blocker), PR #14 (DB-IP fetcher + cron scheduler + cold-start retry budget — the bulk of this sprint's task list), and PR #15 (CI toolchain fix, folded in opportunistically: `golangci-lint-action` v6→v7, go 1.22.2→1.25.11). Sprint goal met. **⚠️ See the dated 2026-07-20 correction on the `behavior.startup_mode` task row below:** one of these six rows was recorded ✅ in error — the `startup_mode` knob was declared-but-unconsumed and was not actually wired until PR #22 (2026-07-20), so this "all six shipped" line overstated the Sprint 3 delivery on that one row.

| Task | Status | Notes |
|---|---|---|
| DB-IP fetcher with ETag / If-Modified-Since | ✅ | Renamed from "MaxMind GeoLite2 fetcher with ETag / If-Modified-Since" (ADR 0003, 2026-07-05 — dropped the MaxMind license-key dependency entirely). PR #14. Fetcher derives the current month, does a plain keyless HTTPS GET of `dbip-country-lite-YYYY-MM.mmdb.gz`, falls back to the prior month on a rollover-day 404, gunzips the single MMDB stream, and writes through the existing `internal/diskcache` path. Conditional GET (ETag/If-Modified-Since) retained — most daily re-fetches of an unchanged monthly file return 304 |
| Cron scheduler for periodic refresh | ✅ | PR #14 |
| Retry with exponential backoff on fetch failure | ✅ | PR #14 |
| Bounded cold-start retry budget | ✅ | PR #14 |
| `behavior.startup_mode: fail-closed \| fail-open` config knob (default fail-closed) | ⚠️ ~~✅~~ → corrected | **⚠️ CORRECTION 2026-07-20 (OQ-FAILOPEN-3).** _Original 2026-07-06 claim (preserved verbatim):_ "The knob itself shipped with Sprint 1's config schema task (`internal/config`); this Sprint 3 task's scope was the fetcher/scheduler's cold-start path actually consulting it, which PR #14 wires in." **This claim is false.** PR #14 never wired the knob. A repo-wide search for `StartupMode` at that time found consumers only in `internal/config` and a single `cmd/bitblocker/main.go` log line — nothing in `internal/server`, `internal/fetcher`, or `internal/scheduler` branched on the value. The knob sat **declared-but-unconsumed** from Sprint 1 until PR #22 actually wired it on 2026-07-20 (ADR 0004 — fail-open at the readiness gate with a mandatory heartbeat). The gap plausibly stayed invisible for ~3 months **because** this row asserted the work was done, so no one looked. This is a dated factual correction, not a re-scoping — the original claim is retained above so the error and its correction are both legible. |
| End-to-end integration tests (fixture MMDB + stub HTTP server) | ✅ | PR #14 |

---

### Decisions Log (historical, through 2026-05-15)

| Date | Decision | Reasoning |
|---|---|---|
| 2026-05-15 | Blocklist swap uses `atomic.Pointer[blocklist.Trie]`, not a pointer-swap under `sync.RWMutex` (ADR 0001). Disk cache stores the raw MMDB file, written via temp-file + `Sync` + atomic rename, with a `Stat`-based staleness bound (`cache.max_age`, default 48h); startup read reuses `mmdb.LoadCountryBlocklist` verbatim (ADR 0002). `Source` type (`Current`/`Swap`) lives in `internal/blocklist`; disk-cache logic lives in `internal/diskcache` to avoid an import cycle (`internal/mmdb` already imports `internal/blocklist`). `go test -race ./...` clean. Landed via PR #8 (ADR 0001 + ADR 0002 + interface spec) + PR #9 (implementation). | Lock-free `atomic.Pointer` is the correct primitive when the payload is immutable after construction and the read:write ratio is extreme (~10^7 reads per refresh on a busy host). `RWMutex` adds shared-cacheline contention to every `/check` for no benefit — `atomic.Pointer.Load()` is a single uncontended atomic read. Go addendum §15 names this case explicitly. Raw-MMDB cache format avoids inventing and versioning a bespoke trie codec; the existing loader is reused as-is. Temp-file + atomic rename is the filesystem analogue of the in-memory atomic swap: a crash at any point leaves either the prior good cache or no cache — never a half-written file. |
| 2026-05-08 | Sprint 2 split into request-path slice (PR #5) and swap+disk-cache slice (separate session) | The five tasks bundled into PR #5 (`/check` + IP extraction + `/healthz` + fail-closed + handler tests) all share the same request-handler code path; landing them together kept the seams coherent and avoided thrashing on the same files across two PRs. The swap+disk-cache slice is the parallel Sprint 2 slice and proceeds against the `Lookup` interface seam — the server defines the interface at the consumer side; today's wiring uses an empty `LookupSource`; the swap slice replaces it with an `atomic.Pointer[blocklist.Trie]`-backed source with no server-side changes. The two slices were intentionally decoupled so each could land thoroughly without one blocking the other; the seam is the contract |
| 2026-04-22 | Cold-start fail mode: fail-closed with guardrails | Authorization gate with no ruleset loaded should default-deny. Disk cache, `startup_mode` config knob, `/healthz` 503, and bounded retry make it operationally tolerable |
| 2026-04-22 | IPv4 + IPv6 supported from Sprint 1 | GeoLite2 ships both; retrofitting the trie later would be painful |
| 2026-04-22 | MaxMind consumed as MMDB binary format (not CSV) **[Provider superseded 2026-07-05 by ADR 0003 — DB-IP "IP-to-Country Lite" replaces MaxMind GeoLite2-Country; the MMDB-binary-not-CSV half of this decision is retained unchanged. See CURRENT § Decisions Log above.]** | Native Go library `maxminddb-golang` exists; avoids custom CSV parsing |
| 2026-04-22 | ASN blocking via BGP.tools deferred from v1 | Data-source access question unresolved; config schema stays forward-compatible (accepts `block.asns`, logs "not implemented" if populated) |
| 2026-04-22 | Malformed `/check` fails closed; header selection is `X-Real-IP` first, then rightmost-XFF | Leftmost XFF is spoofable under Traefik's `trustForwardHeader: true`; `X-Real-IP` reflects the TCP peer Traefik actually saw. Leftmost-XFF support deferred to a future config knob for upstream-CDN scenarios |
| 2026-04-23 | Raw shell pre-commit hook under `scripts/git-hooks/`, activated via `core.hooksPath`, rather than the `pre-commit` framework | Project is solo and Go-only; adding a Python toolchain for one linter is overkill. Migrating to the `pre-commit` framework later is a half-hour job if the contributor surface grows |
| 2026-04-23 | `golangci-lint` pinned to v2.11.4 in CI; local installs run whatever `go install @latest` resolves | Pinning in CI keeps the gate reproducible; leaving local loose avoids forcing contributors through a specific install ritual. If local and CI diverge, bump the CI pin |
| 2026-04-23 | License: MIT | Lowest-friction permissive license for a small self-hosted infra tool; standard in the Go single-binary ecosystem. Apache-2.0's patent grant and NOTICE machinery don't earn their keep here — nothing in the codebase is patentable (CIDR tries, MMDB lookups, forwardAuth shims are decades-old prior art) and the target audience is hobbyist self-hosters, not F500 legal intake. Relicense is possible later if the project ever heads toward a CNCF/Apache-umbrella home |

---

### Open Questions (historical, frozen — narrative as of 2026-07-06 freeze; DB is canonical for current state)

| Question | Owner | Status |
|---|---|---|
| **OQ-1** — MaxMind license key procurement (blocks Sprint 3 fetcher work end-to-end). **Resolved 2026-07-05 (ADR 0003):** dissolved entirely — the GeoIP source switched to DB-IP "IP-to-Country Lite," which needs no account or key. | Jeff | ✅ Resolved |
| **OQ-2** — ASN blocking via BGP.tools — include in v1.x or push to v2? | Jeff | ⬜ Open |
| **OQ-3** — Allowlist feature (exempt admin/monitoring IPs) — v1 or later? | Jeff | ⬜ Open |
| **OQ-4** — Leftmost-XFF config knob for upstream CDN scenarios — when does this become needed? | Jeff | ⬜ Open |
| **OQ-5** — MMDB country match scope: `country.iso_code` only, or also `registered_country.iso_code`? v1 currently matches `country` only; false negatives would be IPs geolocated outside the blocked country but registered inside it. Decide before v1 release; cheap to add later. **Resolved 2026-07-05 (ADR 0003):** DB-IP "IP-to-Country Lite" carries no `registered_country` object, foreclosing the "also match registered_country" option from the data itself. v1 stays `country.iso_code`-only (already the implemented behavior) — Architect-recommended default, accepted by proceeding with DB-IP. | Jeff | ✅ Resolved |
| **OQ-6** — **Reworded 2026-07-06 (ADR 0003 § Consequences → Neutral):** Adopt `maxminddb-golang` v2 reader (optional; needs Go ≥1.24). Previously conflated feature-adoption with closing eight stdlib `govulncheck` findings; those findings are now closed independently via the go 1.25.11 toolchain bump (PR #15, resolves OQ-7). OQ-6 is now purely an optional, decoupled feature-adoption question — it no longer blocks or is blocked by anything security-related. | Jeff | ⬜ Open |
| **OQ-7** — Pre-existing `govulncheck` findings against `go 1.22.2` stdlib. Originally filed for `GO-2025-3750` (Windows-only `os@go1.22.2`); refreshed 2026-05-08 (PR #5) to note eight stdlib findings visible in CI. **Resolved 2026-07-06:** closed by the go 1.22.2→1.25.11 toolchain bump (PR #15) — `govulncheck` scan is now 0 vulns. | DevOps | ✅ Resolved |
| **OQ-8** — `golangci-lint` GitHub Action version: workflow used action `golangci/golangci-lint-action@v6` against pinned linter `v2.11.4`; needed action `@v7` for compatibility. **Resolved 2026-07-06:** action bumped v6→v7 (PR #15). | DevOps | ✅ Resolved |
| **OQ-CACHE-1** — Config-field placement for the disk-cache fields (`cache.path`, `cache.max_age`): dedicated `cache:` YAML block vs. under the existing `behavior:` block. Architect lean: dedicated `cache:` block. Developer implemented a `cache:` block in `config.example.yaml` (PR #9). Proposed 2026-05-15 by Architect (ADR 0002 § Open questions surfaced). | Jeff / Developer | ⬜ Open |
| **OQ-CACHE-2** — On detecting a corrupt or stale cache file at startup, should the daemon remove it (so it does not re-trip the next start's load attempt + WARN), or leave it in place? Architect lean: remove it. Proposed 2026-05-15 by Architect (ADR 0002 § Open questions surfaced). | Developer | ⬜ Open |
| **OQ-CACHE-3** — Sprint 4 / DevOps: the systemd unit needs `CacheDirectory=bitblocker` (creates `/var/cache/bitblocker` owned by the service user and ensures the `bitblocker` user has write permission); the Docker image needs the cache path on a writable volume or `tmpfs`. Out of Sprint 2 scope; record here so Sprint 4 deploy work picks it up. Proposed 2026-05-15 by Architect (ADR 0002 § Open questions surfaced). | DevOps | ⬜ Open |

---

### Carry-over Log (historical)

| Task | Original sprint | Moved to | Reason |
|---|---|---|---|
| ~~Add `.golangci.yml` and wire `golangci-lint` into CI + pre-commit~~ | Sprint 1 | — | Resolved 2026-04-23: `.golangci.yml` + CI job + raw `scripts/git-hooks/pre-commit` in place |

---

## Notes
