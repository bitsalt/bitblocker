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
| Initialize Go module and repo structure (`cmd/`, `internal/…`) per spec | ⬜ | |
| GitHub Actions CI skeleton (build + test on push) | ⬜ | |
| Config schema (YAML) with validation | ⬜ | License key must accept env var override |
| Structured JSON logging setup | ⬜ | |
| CIDR trie supporting IPv4 + IPv6 lookups | ⬜ | |
| Unit tests for trie (insert, lookup, edge cases) | ⬜ | |

---

## Sprint 2 — May 5 to May 18

**Goal:** Daemon answers `/check` and `/healthz` correctly against a preloaded MMDB fixture.

| Task | Status | Notes |
|---|---|---|
| Integrate `maxminddb-golang` reader and populate trie from MMDB | ⬜ | |
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
| LICENSE file | ⬜ | Depends on license confirmation |
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

---

## Open Questions

| Question | Owner | Status |
|---|---|---|
| MaxMind license key procurement (blocks Sprint 3 fetcher work end-to-end) | Jeff | ⬜ |
| ASN blocking via BGP.tools — include in v1.x or push to v2? | Jeff | ⬜ |
| Allowlist feature (exempt admin/monitoring IPs) — v1 or later? | Jeff | ⬜ |
| License confirmation (MIT default) — needed before v1.0 tag | Jeff | ⬜ |
| Leftmost-XFF config knob for upstream CDN scenarios — when does this become needed? | Jeff | ⬜ |

---

## Carry-over Log

| Task | Original sprint | Moved to | Reason |
|---|---|---|---|
| | | | |

---

## Notes
