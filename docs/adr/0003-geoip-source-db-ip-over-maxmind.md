# ADR 0003: Country GeoIP data comes from DB-IP "IP-to-Country Lite," not MaxMind GeoLite2

- **Status:** accepted
- **Date:** 2026-07-05
- **Deciders:** Jeff (operator) — the data-source switch; Architect — the config-surface spec (confirmable at Developer touch time)
- **Amends:** `docs/BitBlocker.md` decisions log 2026-04-22 — "MaxMind consumed as MMDB binary format (not CSV)." The **provider** changes (MaxMind GeoLite2 → DB-IP IP-to-Country Lite); the **MMDB-binary-not-CSV** half of that decision is retained unchanged.
- **Supersedes:** none (ADR-wise)
- **Superseded by:** none

## Context

The v1 country blocklist is built from a GeoIP dataset that maps IP networks to
ISO 3166-1 alpha-2 country codes. The original choice (decisions log 2026-04-22;
spec § Data sources) was **MaxMind GeoLite2-Country**, consumed as an MMDB binary
via `oschwald/maxminddb-golang`.

Two things forced a reconsideration:

1. **MaxMind free-tier friction is blocking Sprint 3.** GeoLite2 requires a free
   account and a per-account license key to download. Procuring that key
   (`bitblocker:OQ-1`) has been the end-to-end blocker on the Sprint 3 fetcher:
   the fetcher cannot be built or tested against a real download without it.
   MaxMind has been non-responsive on free-tier access and is steering users
   toward paid plans. The self-hosted, hobbyist audience this daemon targets is
   exactly the audience that friction turns away.
2. **The spec already anticipated this.** `docs/bitblocker-spec.md` § Open
   questions flagged, before v1, that "MaxMind license key requirement — GeoLite2
   requires registration. Worth evaluating `ip-api.com` or `DB-IP` free-tier
   databases as no-registration alternatives … Decision needed before v1
   release." This ADR is that decision.

The operator has decided to move to a **no-monthly-cost, no-account** source.
The integration point that makes this cheap is already in place: `internal/mmdb/
loader.go` reads exactly one field from the dataset — `country.iso_code` — via
`maxminddb-golang v1.13.1`, and the repo already bundles `maxmind/mmdbwriter`
(test-only) should it ever need to compile its own MMDB from another format. So
the question is narrow: which no-cost source ships an MMDB carrying
`country.iso_code`, with the lightest fetch and license burden?

## Decision

**Adopt DB-IP "IP-to-Country Lite" as the v1 country GeoIP source, replacing
MaxMind GeoLite2-Country.**

- **MMDB binary format is retained.** The 2026-04-22 "MMDB binary, not CSV"
  decision stands — DB-IP IP-to-Country Lite is distributed as an MMDB file, so
  `maxminddb-golang` and the existing loader remain the consumption path. Only
  the *provider* changes. This ADR amends the provider half of that decision and
  leaves the format half intact.
- **DB-IP's MMDB carries `country.iso_code`** (plus `country.geoname_id`,
  `country.is_in_european_union`, `country.names`, and a `continent` object).
  Because `internal/mmdb/loader.go` decodes only `country.iso_code` (its
  `countryRecord` struct deliberately ignores every other field), DB-IP is a
  **drop-in for the loader with zero record-shape change** — see Compatibility
  analysis below.
- **No account, no API key, no cost.** The dataset is fetched from a public,
  dated download URL of the form
  `https://download.db-ip.com/free/dbip-country-lite-YYYY-MM.mmdb.gz`. This
  removes the license-key dependency (`OQ-1`) and simplifies the Sprint 3
  fetcher: a plain HTTPS GET of a gzipped file, no auth, no license-keyed
  permalink.
- **Monthly update cadence.** DB-IP publishes a fresh file monthly
  (~701k records, ~30 MB, current as of June 2026). Country-level national
  allocations move slowly; monthly is adequate for country blocking. This is
  discussed against the existing daily refresh cron under Consequences.
- **License: CC-BY 4.0** (attribution only). BitBlocker is an edge daemon that
  silently drops packets — it does not display geolocation results to end users —
  so the obligation is met by a credit line in the README and a `NOTICE` file.

MaxMind GeoLite2 is not retained as an optional source in v1. Keeping a
license-keyed alternate path alongside the keyless one would re-introduce the
exact config surface (`license_key`, `MAXMIND_LICENSE_KEY`) this change exists to
delete, for a source the operator is deliberately walking away from. If a
GeoLite2 fallback is ever wanted, it returns as its own ADR.

## Compatibility analysis — the loader needs no change

`internal/mmdb/loader.go` decodes each network record into:

```go
type countryRecord struct {
    Country struct {
        ISOCode string `maxminddb:"iso_code"`
    } `maxminddb:"country"`
}
```

DB-IP IP-to-Country Lite's MMDB record carries `country.iso_code` under exactly
that path. The `maxminddb` struct-tag decode (`country` → `iso_code`) resolves
identically against DB-IP's schema. Every other DB-IP field
(`country.geoname_id`, `country.names`, `continent`, …) is simply not decoded, as
today — the struct's minimality is what makes it provider-agnostic.

The loader's traversal machinery is likewise unaffected:
`Networks(SkipAliasedNetworks)`, the `len(net.IP)`-based v4/v6 split in
`prefixFromIPNet`, and the country-set filter all operate on the MMDB network
tree, not on any MaxMind-specific record field. **No change to
`internal/mmdb/loader.go` is required.**

One documentation nit for the Developer (non-functional): the `countryRecord`
doc comment names "the GeoLite2-Country record shape." When the fetcher work
touches this package, update that comment to name DB-IP IP-to-Country Lite. The
code is correct as-is; only the prose is now stale.

A second, deeper consequence of the source's *smaller* schema is surfaced under
Consequences → `OQ-5` interaction: DB-IP does **not** carry a
`registered_country` object, which forecloses one option that was open under
GeoLite2.

## Config-surface change (spec for Developer)

The Developer implements the following in `internal/config` and
`config.example.yaml`. Architect specifies the shape; field-level details are
confirmable at touch time.

### Remove

- The entire `sources.maxmind` block: `enabled`, `license_key`, `edition`.
- The `MAXMIND_LICENSE_KEY` environment-variable override and any code reading
  it (`internal/config` env-merge path).
- MaxMind-specific validation: the `license_key`-present-when-enabled check and
  the `edition` non-empty check both go away with the block.
- The config-file header comment (lines 1–4 of `config.example.yaml`) that
  documents the license-key env var.

### Add

```yaml
sources:
  dbip:
    enabled: true
  bgptools:
    enabled: false
```

- `sources.dbip.enabled` (bool). No key, no URL, no edition — the source needs
  no credentials and the download URL is derived at fetch time (below).
- `bgptools` stays exactly as today (accepted by the schema, `enabled: false`,
  "not implemented" at runtime — unchanged by this ADR).

### Preserve

- **The "at least one source enabled" validation.** It now reads
  `dbip.enabled || bgptools.enabled` instead of `maxmind.enabled || …`. Since
  `bgptools` is not implemented in v1, in practice `dbip.enabled` must be true
  for the daemon to have a working source — but keep the invariant general, do
  not special-case dbip.
- **The fail-closed startup posture** (`behavior.startup_mode`, default
  `fail-closed`) — untouched by this ADR.

### Cache path default

The disk-cache default (ADR 0002 § B) changes from
`/var/cache/bitblocker/GeoLite2-Country.mmdb` to
`/var/cache/bitblocker/dbip-country-lite.mmdb`. Update:

- `cache.path` default in `internal/config` and `config.example.yaml`.
- The `os.CreateTemp` pattern in `internal/diskcache`
  (`GeoLite2-Country.*.mmdb.tmp` → `dbip-country-lite.*.mmdb.tmp`) — cosmetic,
  but keep it consistent with the new artifact name.

The cache *mechanism* (raw-MMDB byte copy, temp-file + `Sync` + atomic rename,
`max_age` staleness bound, reuse of `mmdb.LoadCountryBlocklist` on read) is
unchanged — ADR 0002 holds verbatim. Only the default filename moves.

### URL derivation (no user-facing config)

The download URL is **not** a config field. The fetcher holds an internal URL
template constant and derives the month at fetch time:

```
https://download.db-ip.com/free/dbip-country-lite-%s.mmdb.gz    // %s = "2026-07"
```

Architect lean: keep this out of the YAML. A user-facing URL knob is surface the
operator does not need and can misconfigure. For testability, the fetcher struct
carries an **unexported** base-URL field defaulting to the DB-IP host, which the
Sprint 3 integration tests override to point at a stub HTTP server — the same
consumer-seam discipline used elsewhere in the codebase. No exported/config
surface for it.

## Fetcher implications (Sprint 3)

The Sprint 3 fetcher gets simpler than the MaxMind design assumed:

1. **Derive the current month** `YYYY-MM` (UTC) and format the URL.
2. **Plain HTTPS GET** of `dbip-country-lite-YYYY-MM.mmdb.gz`. No auth header, no
   license-key query param.
3. **Month-rollover fallback.** On the first days of a month the current-month
   file may not be published yet, returning `404`. On a `404` for the current
   month, retry the **prior** month's URL. If both `404`, treat it as a fetch
   failure: retain the existing in-memory blocklist, log `WARN`, and let the
   existing retry budget / backoff (Sprint 3 tasks) apply. Do not fail the
   daemon on a rollover-day miss.
4. **Gunzip.** DB-IP ships a plain gzip stream of a single `.mmdb` — *not* a tar
   archive. This is simpler than MaxMind's `.tar.gz` (which nests the `.mmdb`
   inside a dated directory that must be walked). Decompress the single stream
   straight to bytes; there is no tar member to locate.
5. **Write through the existing disk cache.** Hand the decompressed MMDB bytes to
   the `internal/diskcache` write path (temp-file + `Sync` + atomic rename,
   ADR 0002), then load via `mmdb.LoadCountryBlocklist` and `Source.Swap` in
   (ADR 0001). This is the one existing code path from MMDB bytes → trie; the
   fetcher does not gain a second one.

**Conditional GET still applies.** The Sprint 3 task titled "…with ETag /
If-Modified-Since" stays valuable: DB-IP serves static dated files that should
support conditional requests, so a re-fetch of an already-current month returns
`304` and skips the download+decompress+swap entirely. That task should be
renamed from "MaxMind GeoLite2 fetcher …" to "DB-IP fetcher …" (PM edit,
surfaced as an Open Question below).

## Licensing / attribution

DB-IP IP-to-Country Lite is licensed **CC-BY 4.0**. The attribution obligation is
light for this project because BitBlocker never displays geolocation data to an
end user — it drops packets at the edge. Meeting it:

- A credit line in the **README** and a **`NOTICE`** file, e.g.
  *"IP geolocation data by DB-IP (https://db-ip.com), licensed CC-BY 4.0."*
- BitBlocker's own code stays **MIT** (decisions log 2026-04-23). CC-BY covers
  the *data*, not the software; there is no license-compatibility conflict for an
  edge daemon that bundles no data in its binary and downloads it at runtime.

This lands with the Sprint 4 operator-docs / README task, but the obligation
attaches the moment the fetcher ships, so it is recorded here as a hard
requirement, not a nice-to-have.

## Update cadence vs. the daily refresh cron

The `refresh.schedule` default is `0 3 * * *` (daily). DB-IP publishes monthly.
A daily cron re-fetching a monthly file is **harmless and intentional to leave
as-is**:

- With conditional GET (above), 29–30 of the ~30 daily fetches in a month return
  `304` and are near-free no-ops.
- The one fetch after a month rollover picks up the new file — a daily cadence
  means the new monthly file is adopted within ~24 h of publication, which is
  tighter than a monthly cron would give.
- No config change to `refresh.schedule` is warranted. The operator may lengthen
  it, but the default is fine and the ADR explicitly blesses the
  daily-cron-over-monthly-file arrangement so a future reader does not "fix" a
  non-problem.

## Consequences

### Positive

- **`OQ-1` (license-key procurement) dissolves.** The Sprint 3 fetcher is
  unblocked end-to-end with no operator action — no account, no key, no waiting
  on MaxMind. This is the whole point.
- **Smaller config surface.** `license_key`, `edition`, and the
  `MAXMIND_LICENSE_KEY` env var all leave the codebase, along with their
  validation and the "prefer env so the secret is not on disk" caveat. One fewer
  secret to handle anywhere (config, systemd, docker-compose).
- **Simpler fetcher.** Keyless GET + single-stream gunzip beats license-keyed
  permalink + tar-extraction. Fewer failure modes, less code, easier stub-server
  tests.
- **Zero loader change.** The `country.iso_code`-only decode makes the swap a
  drop-in; the existing `internal/mmdb` tests remain valid against a DB-IP
  fixture.
- **No monthly cost, ever, for the default path** — matching the self-hosted
  audience the product targets.

### Negative

- **`OQ-5` (registered_country match) is partly foreclosed.** DB-IP
  IP-to-Country Lite does **not** carry a `registered_country` object — only
  `country`. Under GeoLite2, `OQ-5` had a live "also match
  `registered_country.iso_code`" option (catching IPs geolocated outside a
  blocked country but *registered* inside it). With DB-IP that option is not
  available from the data. Net effect: v1 country matching is `country.iso_code`
  only — which is already the implemented behavior — but the *upgrade path* for
  `OQ-5` now requires a different or supplementary source rather than a cheap
  loader tweak. This is a genuine capability trade the operator should see; it is
  surfaced (not decided) as an Open Question disposition below.
- **Monthly freshness vs. GeoLite2's twice-weekly.** GeoLite2 updates ~2×/week;
  DB-IP updates monthly. For country-level blocking of slowly-moving national
  allocations this is adequate (and the ADR's cadence section covers the
  mechanics), but it is a real reduction in data freshness. Accepted as an
  appropriate trade for zero-cost, zero-friction access.
- **Attribution obligation appears** (CC-BY 4.0) where GeoLite2's EULA
  attribution was differently shaped. Light burden (README + NOTICE), but it is a
  new must-do that has to actually land in Sprint 4.

### Neutral

- **`OQ-6` (Go 1.24 toolchain bump) is now orthogonal, not superseded.** The
  toolchain bump was entangled with the data source only insofar as
  `maxminddb-golang/v2` (Go 1.24 floor) was a *reader* upgrade. The reader library
  works against DB-IP's MMDB regardless of provider — DB-IP's file is standard
  MMDB — so the v2-reader motivation for the bump is unchanged and undiminished,
  and the security-current motivation (eight stdlib `govulncheck` findings,
  `OQ-7`) is entirely independent of the data source. `OQ-6` stays open on its
  own merits; this ADR simply decouples it from the source decision.
- **`mmdbwriter` (bundled, test-only) is not needed by this change.** DB-IP ships
  a ready MMDB, so there is no compile-our-own step. `mmdbwriter` remains the
  latent capability it was — relevant only to the build-your-own fallback below,
  not to this decision.

## Alternatives considered

All candidates are no-monthly-cost. DB-IP wins on the combination of
drop-in-loader compatibility, no-account fetch, and lightest license.

| Source | Format | Update | Auth to fetch | License | Loader fit | Verdict |
|---|---|---|---|---|---|---|
| **DB-IP IP-to-Country Lite** | MMDB | Monthly | **None** — public dated URL | CC-BY 4.0 (attribution) | **Drop-in** — carries `country.iso_code` | **Chosen** |
| IPinfo Lite | MMDB (+ bundled ASN) | Daily | Free signup **token** required | CC-BY-SA 4.0 | Loader tweak — country field is **not** `country.iso_code` | Rejected for v1 (token friction + loader change); revisit if OQ-2 revives |
| IP2Location LITE DB1 | BIN/CSV native; MMDB available | Monthly | Free signup | CC-BY-SA 4.0 | More adaptation (native format is BIN/CSV) | Weaker fit; rejected |
| Build-your-own (RIR delegation / iptoasn / `sapics/ip-location-db`, compiled via bundled `mmdbwriter`) | MMDB (self-compiled) | We control | None | Varies by input | Full control, but real build + maintenance work | Fallback if DB-IP's terms ever change |

Notes on the runners-up:

- **IPinfo Lite** is the strongest runner-up and the one to revisit if **`OQ-2`
  (ASN blocking)** returns to v1.x/v2 scope: it bundles ASN data alongside
  country in one MMDB with *daily* updates. Its costs for the country-only v1 use
  are a required free signup token (re-introduces the exact account friction we
  are removing) and a country field that is not `country.iso_code` (a loader
  change). Its CC-BY-**SA** ShareAlike bites only on redistribution of the DB
  itself, not on internal use — so it would be usable, just not free of the two
  frictions DB-IP avoids. Worth a forward note in the ASN work.
- **IP2Location LITE DB1**'s native distribution is BIN/CSV; the MMDB build
  exists but the fit is looser and it still wants a signup. No advantage over
  DB-IP for this use.
- **Build-your-own** is the genuine no-vendor option and the documented fallback
  if DB-IP's licensing or availability ever changes: the repo already bundles
  `mmdbwriter`, so compiling RIR delegation files (or `sapics/ip-location-db`) to
  MMDB is a known-feasible path — it just trades vendor dependency for real,
  ongoing build work we do not need to take on while DB-IP serves the need.

## Open items and recommended OQ dispositions

These are **recommendations** for the orchestrator to emit to the tracking DB and
for PM to land in `docs/BitBlocker.md`; Architect does not edit the sprint file.

- **`bitblocker:OQ-1` (MaxMind license-key procurement) → CLOSE / SUPERSEDED.**
  Dropping MaxMind removes the dependency entirely; there is no key to procure.
  Close it, citing this ADR. This unblocks the Sprint 3 fetcher.
- **`bitblocker:OQ-6` (Go 1.24 toolchain bump) → STAYS OPEN, now ORTHOGONAL.**
  The v2-reader and security-current motivations are unchanged and are no longer
  coupled to the data-source choice (see Consequences → Neutral). Keep it open on
  its own merits; drop any framing that ties it to the MaxMind decision.
- **`bitblocker:OQ-5` (registered_country match scope) → SURFACE the
  foreclosure; recommend resolving as `country.iso_code`-only for v1.** DB-IP
  carries no `registered_country`, so the "also match registered_country" option
  is not available from the chosen source. This is product-level (it changes what
  gets blocked), so it is surfaced for Jeff rather than decided here: if
  registered-country matching is genuinely wanted, it requires a supplementary or
  different source and should be its own ADR. Recommended default: accept
  `country.iso_code`-only for v1, which is the current behavior.

Sprint-file edits recommended to PM (via Open Questions):

- Rename the Sprint 3 task "MaxMind GeoLite2 fetcher with ETag / If-Modified-Since"
  to "DB-IP fetcher with ETag / If-Modified-Since," and its note (drop the
  "Depends on MaxMind license key" dependency — now dissolved).
- Add a decisions-log row dated 2026-07-05 recording this source switch, pointing
  here, and noting the MMDB-binary half of the 2026-04-22 decision is retained.
- Update `docs/bitblocker-spec.md` § Data sources and § Open questions (the
  "evaluate DB-IP" question is now resolved) — spec edit, PM/Tech-Writer.
- `.agent-context.md` § Stack / § Key invariants and the milestones/overview
  prose in `docs/BitBlocker.md` reference "MaxMind GeoLite2 data" — orchestrator/
  PM to reword to DB-IP when this ADR is accepted.

## Cross-references

- `docs/BitBlocker.md` decisions log 2026-04-22 (the amended provider decision;
  the retained MMDB-binary decision) and § Open Questions (`OQ-1`, `OQ-5`,
  `OQ-6`).
- `docs/bitblocker-spec.md` § Data sources, § Configuration, § Open questions
  (the "evaluate DB-IP" question this ADR resolves).
- `docs/adr/0002-disk-cache-snapshot-format.md` (the raw-MMDB cache mechanism,
  unchanged; only the default filename moves).
- `docs/adr/0001-blocklist-swap-via-atomic-pointer.md` (`Source.Swap`, the
  publish step the fetcher calls — unchanged).
- `internal/mmdb/loader.go` (`countryRecord` / `LoadCountryBlocklist` — the
  `country.iso_code`-only decode that makes DB-IP a drop-in; doc-comment nit
  noted above).
- `internal/config/config.go` and `config.example.yaml` (the `sources.dbip`
  shape, the removed MaxMind fields and env var, the `cache.path` default).
- `internal/diskcache` (the temp-file pattern rename).
- Coding standards `coding-standards.md` §14 (interface/config surface is a
  versioned contract — removing `license_key`/`edition` is a config-schema change
  operators upgrading across it must know about; note it in the Sprint 4
  release/README notes), §4 (explicit I/O boundaries — the keyless fetch is a
  narrower boundary than the keyed one).

---

*End of ADR 0003.*
