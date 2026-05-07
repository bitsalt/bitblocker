# BitBlocker

> **Back to:** [[BitSalt-Projects]]
> **Started:** 2026-04-21
> **Status:** 🟡 In progress
> **One-liner:** Self-hosted Go daemon that silently drops inbound traffic from specified countries at the Traefik edge, for operators running small-scale self-hosted infrastructure.

---

## Overview

BitBlocker is a single-binary Go daemon that blocks inbound scanning traffic from selected countries before it reaches application code. It sits behind Traefik as a `forwardAuth` middleware, maintains an in-memory CIDR trie populated from MaxMind GeoLite2 data, and refreshes itself on a cron schedule. 

v1 ships country-based blocking with IPv4+IPv6 support and a fail-closed security posture; 

v1.1 adds Prometheus metrics and CLI inspection tools. ASN-level blocking via BGP.tools is deferred until the data-source access question is resolved.

---

## Milestones

| Milestone | Target sprint | Status |
|---|---|---|
| Core engine (CIDR trie + MMDB reader + atomic swap) | Sprint 2 | ⬜ |
| Daemon serves `/check` + `/healthz` with fail-closed cold start | Sprint 3 | ⬜ |
| v1.0 released (binaries + Docker image + operator docs) | Sprint 4 | ⬜ |
| v1.1 observability released (metrics + CLI) | Sprint 5 | ⬜ |

---

## Sprint 1 — Apr 21 to May 4

**Goal:** Repo scaffolded and CIDR trie (IPv4+IPv6) passes unit tests.

| Task | Status | Notes |
|---|---|---|
| Author Go coding-standards addendum at `~/projects/coding-standards/coding-standards-go.md` | ✅ | Pre-Sprint-1 — blocks all coding tasks. Also add reference to `~/.claude/CLAUDE.md` addenda list |
| Initialize Go module and repo structure (`cmd/`, `internal/…`) per spec | ✅ | Module `github.com/bitsalt/bitblocker`; `cmd/bitblocker` + `internal/{blocklist,fetcher,server,config}` stubs, Makefile, `.gitignore`. `make` not yet installed locally — run `go build`/`go test` directly until then |
| GitHub Actions CI skeleton (build + test on push) | ✅ | `.github/workflows/ci.yml`: build, vet, race-enabled tests, `go mod verify`, `govulncheck`. `golangci-lint` deferred — needs `.golangci.yml` (not yet in sprint plan, see Carry-over) |
| Config schema (YAML) with validation | ✅ | `internal/config`: typed structs, `Load`/`Validate`, `MAXMIND_LICENSE_KEY` env override, `behavior.startup_mode` knob, `config.example.yaml`. Full cron-expression validation deferred to Sprint 3 scheduler task (avoids pulling `robfig/cron/v3` before it's used) |
| Structured JSON logging setup | ✅ | `internal/logging`: `log/slog` JSON/text handlers selected from `config.LoggingConfig`, `WithContext`/`FromContext` propagation, discard-logger fallback (no `slog.Default()` reads), `Redact()` with stable 4-byte SHA-256 prefix. Not yet wired into `main` — lands with the HTTP server in Sprint 2 |
| CIDR trie supporting IPv4 + IPv6 lookups | ✅ | Bit-level radix trie, separate v4/v6 roots, built on `net/netip`. Insert masks host bits, is idempotent, ignores invalid/mismatched-family prefixes. Contains normalizes IPv4-in-IPv6 via `Unmap`. Benchmarks: ~39 ns/op IPv4, ~211 ns/op IPv6 against 10k-prefix set — comfortably under the spec's 1ms budget |
| Unit tests for trie (insert, lookup, edge cases) | ✅ | Merged with the trie task per TDD — table-driven coverage in `trie_test.go` for single-host, nested, disjoint, dual-stack normalization, idempotency, invalid-input, and mixed-family `Len()` |

---

## Sprint 2 — May 5 to May 18

**Goal:** Daemon answers `/check` and `/healthz` correctly against a preloaded MMDB fixture.

| Task | Status | Notes |
|---|---|---|
| Integrate `maxminddb-golang` reader and populate trie from MMDB | ✅ | `internal/mmdb/{doc,loader,loader_test}.go`. Pinned `maxminddb-golang v1.13.1` and `mmdbwriter v1.0.0` (test-only) — both pinned below the Go 1.24 floor that newer releases introduced, to stay compatible with the project's `go 1.22.2` toolchain. Loader uses `Networks(SkipAliasedNetworks)` and switches on `len(net.IP)` to build `netip.Prefix` in the form `Trie.Insert` expects. Country match is on `country.iso_code` only — see Open Questions for the `registered_country` scope decision. Lesson `lessons/maxminddb/version-floors-and-aliasing-gotchas.md` written in agent-knowledge-base. PR #2 |
| Atomic swap mechanism (pointer swap under RWMutex) | ⬜ | |
| Disk cache: write snapshot on successful load, read on startup | ⬜ | |
| HTTP server with `/check` endpoint | ⬜ | |
| Client IP extraction: `X-Real-IP` first, rightmost-XFF fallback | ⬜ | |
| `/healthz` endpoint (returns 503 while blocklist empty) | ⬜ | |
| Fail-closed on unparseable `/check` with WARN log | ⬜ | |
| HTTP handler tests | ⬜ | |

---

## Sprint 3 — May 19 to Jun 1

**Goal:** Daemon fetches, refreshes, and survives cold-start failures safely end-to-end.

| Task | Status | Notes |
|---|---|---|
| MaxMind GeoLite2 fetcher with ETag / If-Modified-Since | ⬜ | Depends on MaxMind license key (see Open Questions) |
| Cron scheduler for periodic refresh | ⬜ | |
| Retry with exponential backoff on fetch failure | ⬜ | |
| Bounded cold-start retry budget | ⬜ | |
| `behavior.startup_mode: fail-closed \| fail-open` config knob (default fail-closed) | ⬜ | |
| End-to-end integration tests (fixture MMDB + stub HTTP server) | ⬜ | |

---

## Sprint 4 — Jun 2 to Jun 15

**Goal:** v1.0 tagged with published binaries, Docker image, and operator docs.

| Task | Status | Notes |
|---|---|---|
| Multi-stage Dockerfile producing static binary | ⬜ | |
| systemd unit file | ⬜ | |
| GitHub Actions release workflow (linux/amd64 + linux/arm64) | ⬜ | |
| README with install + config walkthrough | ⬜ | |
| `docs/traefik-integration.md` | ⬜ | |
| LICENSE file (MIT) | ✅ | Added 2026-04-23, ahead of Sprint 4 |
| Tag v1.0 and publish release | ⬜ | |

---

## Sprint 5 — Jun 16 to Jun 29

**Goal:** v1.1 shipped with metrics and CLI inspection tooling.

| Task | Status | Notes |
|---|---|---|
| `/metrics` Prometheus endpoint on separate admin listener | ⬜ | |
| `bitblocker check <ip>` CLI subcommand | ⬜ | |
| `bitblocker list` CLI subcommand | ⬜ | |
| Alert webhook on refresh failure | ⬜ | |
| Tag v1.1 and publish release | ⬜ | |

---

## Decisions Log

| Date | Decision | Reasoning |
|---|---|---|
| 2026-04-22 | Cold-start fail mode: fail-closed with guardrails | Authorization gate with no ruleset loaded should default-deny. Disk cache, `startup_mode` config knob, `/healthz` 503, and bounded retry make it operationally tolerable |
| 2026-04-22 | IPv4 + IPv6 supported from Sprint 1 | GeoLite2 ships both; retrofitting the trie later would be painful |
| 2026-04-22 | MaxMind consumed as MMDB binary format (not CSV) | Native Go library `maxminddb-golang` exists; avoids custom CSV parsing |
| 2026-04-22 | ASN blocking via BGP.tools deferred from v1 | Data-source access question unresolved; config schema stays forward-compatible (accepts `block.asns`, logs "not implemented" if populated) |
| 2026-04-22 | Malformed `/check` fails closed; header selection is `X-Real-IP` first, then rightmost-XFF | Leftmost XFF is spoofable under Traefik's `trustForwardHeader: true`; `X-Real-IP` reflects the TCP peer Traefik actually saw. Leftmost-XFF support deferred to a future config knob for upstream-CDN scenarios |
| 2026-04-23 | Raw shell pre-commit hook under `scripts/git-hooks/`, activated via `core.hooksPath`, rather than the `pre-commit` framework | Project is solo and Go-only; adding a Python toolchain for one linter is overkill. Migrating to the `pre-commit` framework later is a half-hour job if the contributor surface grows |
| 2026-04-23 | `golangci-lint` pinned to v2.11.4 in CI; local installs run whatever `go install @latest` resolves | Pinning in CI keeps the gate reproducible; leaving local loose avoids forcing contributors through a specific install ritual. If local and CI diverge, bump the CI pin |
| 2026-04-23 | License: MIT | Lowest-friction permissive license for a small self-hosted infra tool; standard in the Go single-binary ecosystem. Apache-2.0's patent grant and NOTICE machinery don't earn their keep here — nothing in the codebase is patentable (CIDR tries, MMDB lookups, forwardAuth shims are decades-old prior art) and the target audience is hobbyist self-hosters, not F500 legal intake. Relicense is possible later if the project ever heads toward a CNCF/Apache-umbrella home |

---

## Open Questions

| Question | Owner | Status |
|---|---|---|
| MaxMind license key procurement (blocks Sprint 3 fetcher work end-to-end) | Jeff | ⬜ |
| ASN blocking via BGP.tools — include in v1.x or push to v2? | Jeff | ⬜ |
| Allowlist feature (exempt admin/monitoring IPs) — v1 or later? | Jeff | ⬜ |
| Leftmost-XFF config knob for upstream CDN scenarios — when does this become needed? | Jeff | ⬜ |
| MMDB country match scope: `country.iso_code` only, or also `registered_country.iso_code`? v1 currently matches `country` only; false negatives would be IPs geolocated outside the blocked country but registered inside it. Decide before v1 release; cheap to add later | Jeff | ⬜ |
| Toolchain bump path to unblock `maxminddb-golang/v2` and recent `mmdbwriter` (Go 1.24 floor). Current `go 1.22.2` pin works fine for v1.0; a future feature might want the v2 reader. Separate sprint-level decision | Jeff | ⬜ |
| Pre-existing `govulncheck` finding `GO-2025-3750` on `os@go1.22.2` (Windows-only). Pre-existing on `main`, not introduced by PR #2. Toolchain bump (per the question above) would close it. Decide: suppress / document / bump | DevOps | ⬜ |

---

## Carry-over Log

| Task | Original sprint | Moved to | Reason |
|---|---|---|---|
| ~~Add `.golangci.yml` and wire `golangci-lint` into CI + pre-commit~~ | Sprint 1 | — | Resolved 2026-04-23: `.golangci.yml` + CI job + raw `scripts/git-hooks/pre-commit` in place |

---

## Notes
