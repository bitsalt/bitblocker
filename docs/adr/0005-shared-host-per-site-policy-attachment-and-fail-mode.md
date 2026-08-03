# ADR 0005: On a shared multi-tenant host, policy is selected by per-router middleware attachment, never at the entrypoint; `fail-closed` stays, and the disk-cache guardrail is repaired rather than the posture flipped

- **Status:** proposed
- **Date:** 2026-08-03
- **What unblocks promotion:** `plan-acceptance-gate` — Jeff's acceptance of the app01 BitBlocker deployment plan, of which this ADR is the architecture half (the ansible/deployment half is being produced concurrently by DevOps). Two operator inputs named in § Open questions surfaced (the `block.countries` content, and whether the 15 non-exempt hosting clients have been told their overseas traffic will be dropped) are inputs to that same gate.
- **Deciders:** Architect — the attachment model, the fail-mode analysis, the cache-defect finding, and the growth path. Jeff (operator) — plan acceptance, the country list, and the client-communication question.
- **Amends:** ADR 0004 §E.2 — a factual correction to its claim that the disk cache covers the realistic restart-during-outage overlap. See §3.2 below; the correction block is filed in place in ADR 0004.
- **Interacts with:** ADR 0002 (disk-cache snapshot — §3 depends on its staleness bound and its removal-on-stale behavior), ADR 0003 (the keyless DB-IP fetch that §3's threat path runs through), ADR 0004 (the fail-open readiness gate and its observability contract — unchanged by this ADR except for the §E.2 correction).
- **Supersedes:** none
- **Superseded by:** none

## Context

BitBlocker is a shipped v1.0 tool with no deployment anywhere in the estate
(`docs/deployment.md` opening paragraph). The first deployment target is
**app01**, a single DigitalOcean host running a fleet of sites behind one shared
Traefik v3.6.13 instance (`bitsalt/ansible`, `roles/traefik/defaults/main.yml:14`).

Verified against `bitsalt/ansible` at `site.yml:89-141` on 2026-08-03, the fleet
is **18 site entries across three role families plus umami**:

| Family | Count | Role | Router shape |
|---|---|---|---|
| WordPress | 12 | `roles/wordpress` | Multiple routers per site (apex, `-login`, `-block-batch`, `-www`, `-alias-N`, `-alias-N-www`) |
| Node.js | 5 | `roles/nodejs` | Single router per site; **no `middlewares=` label today** |
| FastAPI | 1 (`portfolio-service`) | `roles/fastapi` | Single router; **no `middlewares=` label today**; carries the `/internal/` GitHub-webhook route |
| Analytics | 1 (`umami`) | `roles/umami` | Single router, `middlewares=hsts-headers@file`; uses the **HTTP-01** ACME resolver `le-http` |

The driving requirement is **binary and per-site**: most hosted sites should get
country blocking; **two hosting clients specifically need outside-the-U.S.
traffic to reach their sites** and must be exempt.

### What the daemon can and cannot do today

Verified against the bitblocker working tree at `main` on 2026-08-03:

- **`block.countries` is a single flat global list** (`internal/config/config.go:72-75`).
  One daemon process carries exactly one policy.
- **`/check` decides from the client IP alone.** `handleCheck`
  (`internal/server/server.go:216-278`) reads only `X-Real-IP` and the rightmost
  parseable `X-Forwarded-For` entry (`internal/server/clientip.go`). Traefik's
  `forwardAuth` sends `X-Forwarded-Host`, but **BitBlocker never reads it** —
  there is no per-host policy selection.
- **Therefore all per-site selection must happen in Traefik**, by choosing which
  routers call `/check` at all.

### The three things this ADR settles

- **A.** How a site opts in or out — the flag, its default, and where the
  middleware is attached. §1–§2.
- **B.** The fail-mode posture on a shared host, where one unusable blocklist
  is a multi-tenant event rather than a single-site one. §3.
- **C.** Whether app01 needs more than one policy now, and what the growth path
  is if it later does. §4.

Rollout, blast radius, and the rollback levers are §5. The Traefik-side contract
DevOps builds against is `docs/interfaces/traefik-middleware-attachment.md`; the
Developer-side contract for the §3 code changes is
`docs/interfaces/cache-freshness-and-stale-fallback.md`. This ADR is the
decision; those are the specs.

---

## Decision

### 1. Attachment is per-router, opt-in, and the flag is required — not defaulted

#### 1.1 The per-site flag

Each site dict in `bitsalt/ansible` `vars/sites/<site>.yml` gains **one required
key**:

```yaml
abitwiser_site:
  enabled: true
  site_name: abitwiser
  geo_filter: enforce        # required; "enforce" | "exempt"
```

**`geo_filter` has no default and the ansible role asserts its presence and its
value.** A site dict that omits it, or carries anything other than the two
literal strings, fails the play with a message naming the site.

This is the deliberate resolution of the fail-safe question, which as posed has
no safe default: *"a new site should not silently acquire a geo policy, nor
silently lose one"* pulls in opposite directions. Any default satisfies one half
and violates the other. **Requiring the key satisfies both** — the only outcome
absence can produce is a loud failure at apply time, before anything changes on
the host. Precedent in the same repo: `roles/traefik/tasks/main.yml:98-107`
deliberately keeps the DNS-01 credential task failing loudly rather than
defaulting, "so a genuine future loss of the token is never silently skipped."

**A string enum, not a boolean**, for three reasons: `grep -c 'geo_filter: exempt'
vars/sites/*.yml` is an auditable count of the exemption set; a typo
(`enforced`, `exempted`) fails the assert instead of coercing; and YAML's
boolean coercion of `no`/`off`/`"false"` cannot silently invert a security
control.

#### 1.2 The middleware is defined once, in the file provider

The `forwardAuth` middleware is defined **exactly once**, in Traefik's file
provider (`/opt/proxy/dynamic/`, rendered by `roles/traefik`), named
`bitblocker`, and referenced from routers as **`bitblocker@file`**. It is *not*
defined via docker labels on the daemon's own container.

Rationale — three properties fall out of the file provider that docker labels do
not give:

1. **One definition point.** The daemon's address, the `trustForwardHeader`
   setting, and the enforcement toggle live in one rendered file, not repeated
   across 18 compose files.
2. **The kill switch (§5.3) works without touching any site.** Traefik watches
   the file-provider directory (`--providers.file.watch=true`,
   `roles/traefik/templates/docker-compose.yml.j2:40`), so a content change to
   that one file is picked up within seconds with **no container restarted and
   no router label changed**.
3. **Consistency with existing practice.** `hsts-headers@file` is already
   defined this way and referenced across the provider boundary.

**The load-bearing constraint on the kill switch:** the middleware named
`bitblocker` must be **defined at all times**, in both the enforcing and the
disabled rendering. A Traefik router that references a middleware which does not
exist is put in an error state and stops serving its rule — so "disable
enforcement by deleting the middleware" would take every attached site down,
which is the opposite of a kill switch. The disabled rendering is a benign
`headers` middleware, not an absent one. Full shape in the interface spec §4.2.

#### 1.3 Which routers carry it

The rule, stated so it survives future router additions:

> **Every router whose rule can terminate at a site's own backend must carry
> `bitblocker@file` when that site is `geo_filter: enforce`. Redirect-only
> routers may omit it. Routers serving machine traffic must never carry it.**

For a WordPress site today that resolves to the **apex router and the `-login`
router**. The `-login` router is the one that must not be missed: its rule is
`Host(domain) && Path(/wp-login.php)` and it has *higher* match priority than
the apex router, so attaching only to the apex leaves the single
highest-value target on a WordPress site — the login form — reachable from
every blocked country. This is not a hypothetical class of error in this repo:
`roles/wordpress/templates/docker-compose.yml.j2:122-138` and `:140-154` both
carry comments documenting a control that was inert in production for months
because a per-router attribute was missing on exactly these two routers.

`-www` and `-alias-N*` routers are `redirectregex` only; a blocked-country
client following the redirect lands on the apex router and is blocked there. One
wasted round trip, no bypass. `-block-batch` already returns 403 to everything
via a TEST-NET `ipAllowList`; adding a `forwardAuth` call in front of an
unconditional 403 buys nothing.

**Routers that must never carry it**, because they serve machine traffic that
does not originate where a human visitor does:

- **The `web` entrypoint / ACME HTTP-01 path.** The `le-http` resolver
  (`roles/traefik/templates/docker-compose.yml.j2:65-68`) validates over port 80
  from Let's Encrypt's vantage points, which are multi-regional by design.
  Geo-filtering that path breaks certificate renewal — and breaks it silently,
  60 days before anyone notices. The DNS-01 resolver `le` is unaffected (no
  inbound HTTP).
- **`portfolio-service`'s `/internal/` webhook routes.** GitHub's webhook
  egress is already constrained by an `ipAllowList` (`internal-ip-allowlist`);
  layering a geo filter over it adds a second, less legible failure mode for a
  push-triggered sync.
- **Any uptime monitor or external health probe path** — see § Open questions.

#### 1.4 Never at the entrypoint — and this is the load-bearing negative

The all-sites lever exists
(`--entrypoints.websecure.http.middlewares=hsts-headers@file`,
`roles/traefik/templates/docker-compose.yml.j2:50`) and is superficially
attractive: one attach point, no router enumeration, no drift, and it would
automatically cover every future router. **It is rejected.**

The decisive reason is not the exemption requirement (which could be met by the
§4.2 host-selection feature). It is **blast radius under daemon failure**:

- Traefik's `forwardAuth` to an unreachable address returns **500 for the
  original request**. A BitBlocker process that is stopped, crashed, OOM-killed,
  mid-`docker pull`, or wedged takes down **every router on the entrypoint** —
  including the two exempt clients who are paying specifically not to be subject
  to this, including `portfolio-service`, including umami, including any site
  that was never meant to be filtered at all.
- Under per-router attachment, that same daemon failure is confined to the
  sites that opted in. The exempt clients are **structurally** immune, not
  immune-by-config-lookup-inside-the-failing-component. That distinction is the
  whole point: an exemption that depends on the exempting process being alive
  is not an exemption.

Secondary reason: entrypoint attachment would put the geo filter in front of the
`websecure` entrypoint's every router, which is a superset of §1.3's "must never
carry it" list.

The cost of rejecting it is honest and stated: **per-router attachment can
drift.** A router added later without the label is silently unprotected. §1.3's
rule and the interface spec's enumeration are the mitigation; the verification
in §5.2 is the detection.

---

### 2. Fail-mode posture: `fail-closed` stays. Do not set `fail-open` on app01.

This is the decision with the most at stake, so the analysis is given before the
recommendation.

#### 2.1 What "unusable" actually costs on a shared host

One daemon serving ~16 filtered sites means the unusable-blocklist state is a
multi-tenant event. Under `fail-closed`, every attached site returns the
configured block status (403) to **every visitor, from every country**, for the
duration. That is a real, correctly-feared outcome and it deserves the
arithmetic rather than a posture preference.

#### 2.2 The realistic paths to "unusable", with their windows

`UNUSABLE ⇔ lookup == nil || lookup.Len() == 0` (interface spec
`fail-open-and-readiness.md` §2.1). Verified against the code, there are exactly
three ways to reach it, and one of them is far more likely than ADR 0004
assumed.

**Path 1 — a failed refresh on a running daemon. Cannot happen.** Every failure
path in `fetcher.Refresh` returns before `f.source.Swap(trie)`
(`internal/fetcher/fetcher.go:139-170`). `Swap` is the only mutator and is
called only after a fully successful load. A daemon whose refreshes have been
failing for a month keeps serving its last good trie. **Window: none. This path
is closed.**

**Path 2 — `ready-then-empty`: `block.countries` matches nothing in the
dataset.** A typo'd or nonexistent country code produces a valid MMDB load with
`Len() == 0`. This is a config error, is caught immediately by the
`ever_ready: true` + `likely_cause` heartbeat (ADR 0004 §D.2), and is fixed by
correcting the config. **Window: until the operator reads the ERROR log.**
Bounded by verification (§5.2), which will catch it before any site is attached.

**Path 3 — cold start with no usable cache.** This is the one that matters, and
the mechanism is worse than ADR 0004 §E.2 recorded.

#### 2.3 The cache-freshness defect — the finding that changes the analysis

ADR 0004 §E.2 asserts:

> "ADR 0002's cache (default `max_age` 48h) means a routine restart during a
> DB-IP outage serves cached data and never reaches the predicate at all. The
> uncovered window is a restart after >48h of both downtime and fetch failure."

**That bound does not hold in the steady state.** Traced through the code:

1. Staleness is measured against the cache file's **modification time**
   (`internal/diskcache/cache.go:108` — `now.Sub(info.ModTime()) > maxAge`).
2. The mtime advances **only** when `Refresh` takes the `OutcomeUpdated` branch
   and calls `diskcache.Write` (`internal/fetcher/fetcher.go:157`). A `304 Not
   Modified` returns at line 147-150, **before any cache write**.
3. The fetcher sends conditional-GET validators (`If-None-Match` /
   `If-Modified-Since`) from `f.etag` / `f.lastModified`
   (`internal/fetcher/fetcher.go:216-221`), populated on the last successful
   download.
4. DB-IP publishes **monthly** (ADR 0003; `dbip-country-lite-YYYY-MM.mmdb.gz`).
   The default refresh cadence is **daily** (`0 3 * * *`,
   `internal/config/config.go:163`).

So a healthy, long-running daemon downloads once, then gets `304` on every
subsequent daily fetch for the rest of the month. **The cache file's mtime is
pinned to the last actual download and ages past the 48h `max_age` within two
days** — after which the cache is stale for the remaining ~28 days of the month.

And on the next restart, `loadDiskCache` does not merely skip a stale cache — it
**deletes it** (`cmd/bitblocker/main.go:195-199`, `removeUnusableCache`,
OQ-CACHE-2 ratified 2026-07-19). The daemon then starts UNUSABLE with no cache
on disk to fall back to, and the deletion is not undone by restarting again.

The corrected picture of Path 3:

| Step | Outcome |
|---|---|
| Restart at any time >48h after the last actual download | Cache is stale → **deleted** → daemon starts UNUSABLE |
| Cold-start fetch succeeds (validators are empty after restart, so this is an unconditional GET → 200 → full download → cache rewritten) | Window is **seconds**. This is the overwhelmingly common case. |
| Cold-start fetch fails | 8 attempts, backoff `2·2ⁿ` capped at 5m ⇒ **~4m14s of retries** (`internal/scheduler/scheduler.go:19-23, 164-194`), then the budget is exhausted and the daemon **waits for the next cron tick — up to ~24h** |

**The uncovered window is therefore not "a restart after >48h of both downtime
and fetch failure." It is "any restart that coincides with a DB-IP fetch
failure,"** because the cache is stale-and-therefore-deleted for most of any
given month. On a host where `restart: unless-stopped` plus an ansible re-apply
plus a host reboot are all routine, that is a materially more likely event than
ADR 0004 costed.

**One conditional, stated honestly.** The whole mechanism depends on
`download.db-ip.com` actually serving `ETag` and/or `Last-Modified`. If it
serves neither, `f.etag`/`f.lastModified` stay empty, every daily fetch is
unconditional, every fetch rewrites the cache, and the mtime is never more than
24h old — the defect does not exist. **This is one command to settle and it is a
prerequisite to the fix, not to this ADR:**

```
curl -sSI https://download.db-ip.com/free/dbip-country-lite-2026-08.mmdb.gz | grep -iE '^(etag|last-modified):'
```

The §2.5 remedy is correct and harmless in both cases; the verification decides
whether it is urgent or merely tidy.

#### 2.4 What `fail-open` would trade away, and why it is the wrong instrument

`fail-open` would convert the Path-3 tail from "16 sites down" into "16 sites
unfiltered." That is a real improvement on availability and a real regression on
the thing the daemon exists for, and on a shared host the regression is worse
than it looks:

- The failure becomes **silent to visitors and to clients**. Nobody reports it.
  Detection collapses onto one channel — an ERROR log heartbeat — on a host
  whose alerting on that channel does not exist yet (§ Open questions).
- The state is **sticky**. The same restart-plus-fetch-failure that opens the
  window also leaves no cache on disk, so the daemon can sit `ever_ready: false`
  indefinitely, allowing everything, while `/healthz` 503s into a monitor
  nobody has wired.
- It is the **decorative-control** outcome. The 2026-07-21 fleet compromise on
  this same host (`roles/wordpress/templates/docker-compose.yml.j2:79-94`) is
  the record of what a security control that was quietly not working costs on
  app01.

And critically: **`fail-open` would be compensating for a defect, not for a
posture.** Flipping the posture to paper over a cache that is not doing its job
leaves the cache not doing its job, and buys the availability with the daemon's
entire purpose. ADR 0004 §E already refused the loud-to-silent trade, for
reasons that hold *more* strongly on a shared host, not less: sixteen tenants
make the silent failure harder to notice, not easier.

**ADR 0004's rejection of time-bounded fail-open also stands and is not
reopened.** Its reasoning — that a posture which is a function of elapsed time
rather than of configuration is illegible to an operator debugging live, and
that it reintroduces the 403s at the worst moment — is correct and this ADR
adopts it unchanged.

#### 2.5 The third option that *is* warranted: serve stale rather than serve nothing

There is a better lever than either posture, and it does not touch
`startup_mode` at all. Two changes, in priority order:

**Fix A (required before the app01 rollout) — make `cache.max_age` mean what
ADR 0002 says it means.** On `OutcomeUnchanged` (a 304), the daemon has just
received affirmative upstream confirmation that its cached bytes *are* the
current published artifact — evidence of freshness at least as strong as a 200.
Advance the cache file's mtime at that moment (`os.Chtimes`). A healthy daemon's
cache then never exceeds one refresh interval in age, every routine restart
finds a fresh cache, and the uncovered window collapses to exactly the bound
ADR 0004 §E.2 claimed but did not have: *a restart after >48h of continuous
fetch failure.*

This is a **defect fix**, not a feature: it repairs a guardrail that the
fail-closed posture was explicitly costed against.

**Fix B (recommended, v1.1) — a stale cache becomes a last-resort fallback
instead of being deleted.** Today `ErrStale` ⇒ skip **and delete**. Instead:
retain the stale file, and if the cold-start retry budget is exhausted with no
usable blocklist, load it and serve it with a loud, recurring ERROR naming its
age. Month-old geo data is materially better than no geo data: its failure mode
is a handful of reassigned prefixes, versus a total outage of every attached
site. This preserves the fail-closed contract exactly — the daemon still never
allows traffic it cannot evaluate — while removing the 24h tail.

Both are specified implementation-ready in
`docs/interfaces/cache-freshness-and-stale-fallback.md`. **Neither is
implemented here.** Fix A is a Developer pass that should land before the
rollout reaches Phase 2; Fix B is Sprint 5 scope.

#### 2.6 The recommendation, stated plainly

**Deploy app01 with `behavior.startup_mode: fail-closed` (the default). Do not
set `fail-open`.** Land Fix A first. Take Fix B in Sprint 5.

The shared-host multiplier is a real concern and the answer to it is **not** a
posture flip — it is the three structural bounds this ADR puts around the
failure, none of which is a config knob:

1. **Fix A** removes the common path to the failure (§2.5).
2. **Per-router attachment** (§1.4) bounds who is affected when it happens: the
   exempt clients and every unfiltered service are untouched by any BitBlocker
   failure, including a total process failure.
3. **The file-provider kill switch** (§5.3) turns a 16-site outage into a
   ~10-second single-file edit that restarts nothing and needs no ansible run.

Availability on this host is bought with blast-radius containment and a fast
rollback, not by asking the security control to stop working when it is
confused.

---

### 3. One policy, one daemon. No second instance for app01.

Jeff's stated requirement is binary — filtered / not filtered — and per-router
attachment expresses exactly that with a single daemon and no code change.
**A second policy is not built, not staged, and not designed for.** (KISS;
coding-standards §15 YAGNI.)

#### 3.1 Growth path, tier 1: N policies = N daemon instances

If app01 later needs two distinct country lists, the answer is a second
BitBlocker container with its own config, its own port, its own cache volume,
and its own file-provider middleware (`bitblocker-strict@file`). Sites reference
whichever middleware their policy calls for. The daemon is a single small
static binary with a ~30 MB dataset; two or three instances is a non-event.

This scales cleanly to roughly 3–4 policies. Past that the per-instance cache
and fetch duplication, and the operator burden of keeping N configs coherent,
start to argue for tier 2.

#### 3.2 Growth path, tier 2: `X-Forwarded-Host` policy selection — **specced, and explicitly NOT in scope for app01**

This is the feature that would let one daemon carry a policy table keyed by
hostname. It is written up here so it is not re-derived later, and because its
security analysis has a **blocking prerequisite that is not currently
satisfied.**

**Shape.** A new config block, additive, with the existing `block.countries`
retained verbatim as the default policy:

```yaml
block:
  countries: [CN, RU]          # unchanged — the DEFAULT policy

policies:                       # new, optional; absent ⇒ today's behavior exactly
  default: block.countries      # implicit; not written
  hosts:
    exempt-client-a.com: none   # named policy "none" = allow all
    www.exempt-client-a.com: none
    exempt-client-b.com: none
```

`/check` reads `X-Forwarded-Host`, normalizes it (lowercase, strip port, strip
trailing dot, punycode form), looks it up, and applies the matched policy;
an unmatched or absent host falls to the default policy.

**Contract rules that are not negotiable if this is ever built:**

1. **An unmatched host applies the default policy — never "no policy."** The
   fail direction for a hostname the operator forgot to list must be *more*
   filtering, not less. Consequence, stated up front: an exempt client whose
   `www.` or alias hostname is omitted gets filtered, which is a **loud** failure
   (the client calls) rather than a silent one (a site quietly stops being
   protected). That asymmetry is the reason for the rule.
2. **The default policy stays `block.countries`,** so a config with no
   `policies:` block behaves byte-identically to today. Additive extension,
   coding-standards §14.
3. **`X-Forwarded-Host` is never consulted for the unparseable-client-IP
   branch.** ADR 0004 §A.1's carve-out is untouched: input validity is not
   data availability, and it is not policy selection either.
4. **A `/healthz` field must expose the loaded policy count**, or an operator
   cannot tell a policy table that failed to load from one that matched nothing.

**Security analysis.**

The instinctive objection is that the Host header is client-controlled. In the
normal case that objection is weaker than it looks: Traefik routed the request
by matching `Host(...)` against the very same value it then puts in
`X-Forwarded-Host`. A client claiming `Host: exempt-client-a.com` is routed to
exempt-client-a's backend and exempted — which is precisely the configured
policy for that site. The exemption is *coextensive with the routing decision*,
so a "spoof" buys the attacker nothing they could not get by simply visiting the
exempt site.

**The real problem is `trustForwardHeader`.** BitBlocker's client-IP contract
requires `trustForwardHeader: true` on the `forwardAuth` middleware
(`docs/traefik-integration.md` § Register the middleware) — and that setting is
about whether Traefik **passes through** the incoming `X-Forwarded-*` headers
rather than overwriting them with its own. If, under `trustForwardHeader: true`,
Traefik forwards a *client-supplied* `X-Forwarded-Host` in preference to the one
derived from `req.Host`, then the header is genuinely attacker-controlled and
**decoupled from the routing decision** — at which point any client from a
blocked country appends `X-Forwarded-Host: exempt-client-a.com` to a request for
a filtered site and walks straight through. That is a total bypass of the
control.

**Therefore: this feature is blocked on an empirical determination**, against the
deployed Traefik version, of whether `trustForwardHeader: true` causes a
client-supplied `X-Forwarded-Host` to reach the auth server. It is not a design
question and must not be settled by reading documentation. Until it is settled
and, if necessary, mitigated (e.g. by requiring Traefik to strip inbound
`X-Forwarded-Host` at the entrypoint), the feature does not enter a sprint.

**Not in scope for the app01 deployment, for four independent reasons:** the
requirement is binary and tier-0 already serves it; it needs a code change on a
released binary; the blocking verification above is unresolved; and — decisively
— it only pays off in combination with entrypoint-level attachment, which §1.4
rejects on blast-radius grounds regardless.

---

### 4. Rollout, blast radius, and rollback

#### 4.1 Phased rollout

| Phase | Scope | Blast radius if wrong |
|---|---|---|
| **0** | Daemon deployed and running on app01. **Zero routers attached.** File-provider middleware rendered in the *disabled* form. | **None.** Nothing calls `/check`. |
| **1** | Attach to **one BitSalt-owned site** (never a client site). Middleware flipped to enforcing. | One BitSalt property. |
| **2** | Attach to the remaining `geo_filter: enforce` WordPress sites in one apply. | The enforcing WordPress set. |
| **3** | Node.js / FastAPI sites, if in scope — requires new `middlewares=` plumbing in those roles, which does not exist today. | Those sites. |

The two exempt clients are `geo_filter: exempt` and are **never attached at any
phase**. `portfolio-service`'s `/internal/` routes and the ACME HTTP-01 path are
never attached at any phase (§1.3).

Phase 0 is not a formality — it is where the header contract gets proven, and it
costs nothing to run for a day.

#### 4.2 Verification that the control is not decorative

Three checks, in order. The third is the one most likely to be skipped and is
the one that catches the failure mode with precedent on this host.

**V1 — daemon in isolation (Phase 0, from app01 itself):**

```
curl -s -o /dev/null -w '%{http_code}\n' -H 'X-Forwarded-For: <IP in a blocked country>' http://<daemon>:8080/check   # expect the block status
curl -s -o /dev/null -w '%{http_code}\n' -H 'X-Forwarded-For: <US IP>'                    http://<daemon>:8080/check   # expect 200
curl -s http://<daemon>:8080/healthz                                                                                   # expect 200, "serving":"enforcing", prefixes > 0
```

Then restart the daemon and re-check `/healthz` — it must return to 200 without
a visible window, which is the observable proof that Fix A landed and the cache
is being used.

**V2 — end to end through the edge (Phase 1), using a canary country.** Testing
the real list requires an egress in a blocked country, which is usually not
available. Instead, temporarily add to `block.countries` a country you *can*
egress from (a VPN endpoint, a cheap VPS), confirm the pilot site returns the
block status through Traefik on port 443 from that egress, confirm a US egress
still returns 200, then remove the canary and re-verify. This proves the whole
chain — Traefik's header handling, the middleware attachment, the daemon's
decision — rather than the daemon alone.

**V3 — the inert-control check.** BitBlocker takes the **rightmost** `X-Forwarded-For`
entry. If Traefik's `forwardAuth` call presents XFF as `<client>, <traefik-internal-IP>`
rather than a bare `<client>`, the rightmost entry is Traefik's own address,
which is in no blocked country, and **the daemon allows everything while looking
perfectly healthy** — 200s on `/healthz`, a populated trie, no errors. The
control would be entirely decorative and nothing in V1 would reveal it.

Detect it by enabling `behavior.log_blocked` (default true) and, at Phase 1,
also `behavior.log_allowed` temporarily, then confirming the redacted client
addresses in the daemon's logs **vary across requests** rather than being one
constant `172.21.x.x`. A constant internal address is the signature of this
failure. Do not proceed to Phase 2 without this check.

The header contract is believed clean on app01 — Traefik scopes
`forwardedHeaders.trustedIPs` to the docker-proxy subnet
(`roles/traefik/templates/docker-compose.yml.j2:44,46`), so an untrusted public
peer's own XFF is discarded and rewritten from the real TCP peer, and
`ansible/lessons/umami-geo-granularity-client-ip-header.md` independently
confirms nothing fronts app01. **That is a reason to expect V3 to pass, not a
reason to skip it.** A CDN placed in front of one site later would invalidate it
silently; that is a named residual risk, not a current condition.

#### 4.3 Rollback levers, in order of speed

1. **Fleet-wide, ~10 seconds, no restarts:** set `bitblocker_enforcing: false`
   and render the file-provider fragment in its disabled form. Traefik's file
   watcher picks it up; no container is restarted, no router label changes, no
   ansible run against site roles. This is the incident lever.
2. **Per-site, minutes:** `geo_filter: exempt` on one site, re-apply that site's
   role.
3. **Full removal, one apply:** all sites to `exempt`, remove the daemon.

**The anti-lever, named because the instinct under incident is exactly wrong:**
**do not stop or `docker rm` the BitBlocker container to "turn it off."** A
`forwardAuth` middleware pointing at a dead address makes Traefik return **500
for every attached site** — stopping the daemon converts a partial problem into
a total outage of everything it was attached to. Lever 1 exists precisely so
that this is never the reachable option. It belongs in the runbook in these
words.

#### 4.4 The daemon must not have a container HEALTHCHECK

`roles/wp_container_autoheal` installs a host-side timer that runs
`docker ps --filter health=unhealthy` and restarts **any** container on app01
reporting unhealthy (`roles/wp_container_autoheal/templates/autoheal.sh.j2:41-53`).
It is fleet-scoped, not WordPress-scoped.

If BitBlocker's container declares a `HEALTHCHECK` wired to `/healthz`, this
composes into a genuinely harmful loop: an unusable blocklist ⇒ `/healthz` 503
(deliberately, ADR 0004 §C) ⇒ container marked unhealthy ⇒ autoheal restarts it
⇒ the restart re-runs `loadDiskCache`, which **deletes** a stale cache and resets
the cold-start retry budget ⇒ still unusable ⇒ restart again at the cooldown
interval. Every iteration destroys recovery state and buys nothing, because
**restarting a daemon that has no blocklist does not give it a blocklist.**

`/healthz` is a **readiness** signal, not a liveness signal — ADR 0004 §C says
this in as many words. Therefore:

> **BitBlocker's container declares no `HEALTHCHECK`.** Readiness is monitored
> externally (`/healthz` polled from the host, plus alerting on the ERROR
> heartbeat via the existing Vector→Loki path). Nothing restarts the daemon on
> a `/healthz` 503.

The shipped image is distroless with no shell or fetch tool, so the naive
`HEALTHCHECK` does not work anyway (`docs/traefik-integration.md` § `/healthz`).
This constraint exists to stop a well-intentioned wrapper image from
reintroducing it.

---

## Consequences

### Positive

- **The exemption is structural.** The two exempt clients are unaffected by any
  BitBlocker state — unusable blocklist, wedged process, stopped container,
  bad country list. Their sites never call `/check` at all. No config lookup
  inside a possibly-failing daemon stands between them and their traffic.
- **A defect in a shipped guardrail is found and named** (§2.3). `cache.max_age`
  has been effectively inoperative in the steady state since the fetcher landed,
  and ADR 0004's security analysis was costed against a bound the code does not
  provide. Finding this before the first deployment rather than during the first
  incident is the whole return on doing the design pass.
- **The fail-closed posture is preserved without asking anyone to accept a 24h
  outage tail.** Fix A closes the common path; Fix B closes the tail; neither
  weakens the contract that the daemon never allows traffic it cannot evaluate.
- **The incident lever restarts nothing.** A one-file change picked up by
  Traefik's watcher, with no container recreated, is the cheapest rollback
  available on this host and is available even when ansible is not.
- **The `-login` router bypass is caught at design time.** Attaching only to the
  apex router — the obvious reading of "attach the middleware to the site" —
  would have left WordPress login reachable from every blocked country.
- **The autoheal interaction is caught at design time.** The obvious "wire the
  healthcheck to `/healthz`" would have produced a restart loop that destroys
  cache state on a host that already runs a fleet-wide autoheal timer.

### Negative

- **Per-router attachment can drift.** A router added later without
  `bitblocker@file` is silently unprotected, and this repo has a documented
  history of exactly that class of error on exactly these routers. Mitigated by
  §1.3's rule and the interface spec's enumeration; detected only by
  verification, which is a periodic cost rather than a structural guarantee.
  This is the price of rejecting the entrypoint lever and it is accepted
  knowingly.
- **Node.js and FastAPI sites need new plumbing.** Neither role emits a
  `middlewares=` label today, so Phase 3 is genuinely new template work rather
  than adding a name to an existing list. If those sites are in scope, that cost
  is real.
- **A required `geo_filter` key is a breaking change to every site vars file.**
  All 18 must be edited before the next full apply, and forgetting one fails the
  play. That loudness is the design intent, but it is friction, and it means the
  ansible change cannot land piecemeal.
- **Fix A is a code change to a released binary,** which means a Developer pass,
  a QA pass, and (per the project's own Sprint 4 lesson) a rehearsed tag before
  the rollout reaches Phase 2. The deployment is not purely a DevOps exercise.
- **`block.countries` remains a single global list.** If the fleet later needs
  two policies, §3.1's second instance is operationally clumsy (two configs, two
  caches, two sets of logs to correlate) and §3.2 is blocked on an unresolved
  security verification.

### Neutral

- **Redirect-only routers are deliberately left unattached.** A blocked-country
  client following a `www.` redirect is blocked one hop later at the apex. The
  extra round trip is not worth 4 more attachment points per site.
- **The disabled kill-switch rendering adds a request header to backends.** It
  is inert to WordPress, Node, and FastAPI, and doubles as a positive signal in
  backend logs that enforcement is off — which is more useful than the absence
  of a signal.
- **Phase 0 has no user-visible effect at all,** and is therefore easy to skip.
  It is where the header contract is proven; skipping it moves that discovery
  into Phase 1, where a site is live behind it.

---

## Alternatives considered

### Attach at the `websecure` entrypoint and exempt inside BitBlocker

One attach point, immune to router drift, automatically covers every future
router. Needs §3.2's host-selection feature to express the exemption.

**Why not:** blast radius. `forwardAuth` to an unreachable address is a 500, so
any BitBlocker process failure takes down every router on the entrypoint —
including the two exempt clients, `portfolio-service`, and umami. An exemption
implemented *inside* the component whose failure you are worried about is not an
exemption. It also puts the geo filter in front of the ACME HTTP-01 path and the
webhook routes (§1.3's never-attach list). This is the closest alternative to
being right and it is rejected on a single decisive property; recorded at length
because its drift-immunity is genuinely attractive and it will be re-proposed.

### A boolean `geo_block: true/false` with a default

Simpler, matches the `enabled:` convention already in the site dicts.

**Why not:** every possible default violates one half of the stated fail-safe
requirement, and YAML boolean coercion (`no`, `off`, `"false"`) can silently
invert a security control. A required string enum makes omission a loud apply-time
failure and makes the exemption set countable with `grep`.

### Define the middleware via docker labels on the daemon's container

Consistent with how the per-site WordPress middlewares are defined.

**Why not:** it puts the kill switch behind a container recreate, and it couples
the middleware's existence to the daemon container's lifecycle — so the
enforcement toggle and the thing being toggled fail together. The file provider
gives a definition that survives the daemon and a change that Traefik picks up
without restarting anything.

### Set `startup_mode: fail-open` on app01

Trade filtering for availability on a host with 16 filtered tenants.

**Why not:** §2.4. It converts a loud failure into a silent one on a host where
detection is a single unwired log channel, it makes the failure sticky (no cache
left on disk to recover from), and it compensates for a defect (§2.3) instead of
fixing it. ADR 0004 §E made this trade-off call once already; the shared-host
multiplier strengthens its reasoning rather than overturning it.

### Time-bounded fail-open (allow for N minutes after cold start, then revert)

Cap the exposure by treating fail-open as a startup grace period.

**Why not:** ADR 0004 § Alternatives rejected this and that rejection stands
unchanged. A posture that is a function of elapsed time rather than of
configuration is illegible to an operator debugging live, and it reintroduces
the 403s at the worst moment. It is listed here only to record that it was
reconsidered under the shared-host framing and the answer did not change.

### Raise `cache.max_age` to 720h instead of fixing the mtime

Set the max age past a month so the never-advancing mtime stops mattering.

**Why not:** it makes `max_age` mean nothing rather than making it mean
something, and it silently accepts serving month-old geo data on a routine
restart — which is the exact outcome ADR 0002 § Alternatives ("No staleness
bound — serve any loadable cache") rejected. Fix A costs one `os.Chtimes` call
and preserves the bound's meaning; Fix B is the *deliberate*, loudly-logged
version of "serve stale," gated on the retry budget being exhausted rather than
applied unconditionally.

### Run BitBlocker as a systemd unit on the host instead of a container

The shipped `packaging/systemd/bitblocker.service` has `DynamicUser=true`,
`CacheDirectory=bitblocker`, and a full hardening set, and it sidesteps §4.4's
autoheal interaction entirely.

**Why not (for app01):** it is genuinely defensible and is DevOps's call, not
this ADR's. The container path matches how everything else on app01 is deployed
and keeps the daemon on the `proxy` network with no published port. Recorded so
DevOps can choose the systemd path deliberately if the host-side ergonomics
favor it; the only constraints this ADR places on that choice are the cache
persistence requirement, the never-publicly-reachable requirement, and §4.4.

---

## Open questions surfaced

- **OQ-A (Jeff, operator input — blocks Phase 1).** The content of
  `block.countries`. This is a business decision about which markets the hosted
  sites serve, not an architecture one, and this ADR deliberately does not pick
  it. Needed as a concrete ISO 3166-1 alpha-2 list before any site is attached.
- **OQ-B (Jeff, product-level — blocks Phase 2, and is a stop condition).** The
  ~15 non-exempt sites belong to hosting clients too. Attaching a country filter
  drops their overseas visitors, including any legitimate ones — expatriate
  customers, overseas staff reaching wp-admin, referral traffic. **Have those
  clients been told, and is the decision BitSalt's to make on their behalf?**
  This is a client-commitment question and the architecture pass will not decide
  it. Note that OQ-3 (per-IP allowlist for admin/monitoring addresses) is still
  open and unbuilt, so there is **no mechanism today** to except a named
  overseas address from an otherwise-filtered site — the only granularity
  available is whole-site.
- **OQ-C (Jeff/DevOps — blocks Phase 1).** Which site is the Phase-1 pilot. Must
  be BitSalt-owned, not a client site. `bitsalt-staging` is the natural choice
  but is a Node.js site, which needs Phase-3 plumbing first; the pragmatic pick
  is a low-traffic BitSalt-owned WordPress property.
- **OQ-D (DevOps — blocks Fix A's priority, not the ADR).** Does
  `download.db-ip.com` return `ETag` and/or `Last-Modified`? One `curl -sSI`
  (§2.3) settles whether the cache-freshness defect is live or latent.
- **OQ-E (DevOps/Developer — blocks §3.2 only, not this deployment).** Under
  `trustForwardHeader: true` on Traefik v3.6.13, does a client-supplied
  `X-Forwarded-Host` reach the `forwardAuth` target, or does Traefik overwrite
  it from `req.Host`? Must be settled empirically, not from documentation.
  §3.2 does not enter a sprint until it is.
- **OQ-F (DevOps).** Is there any external uptime monitor, synthetic check, or
  third-party integration hitting these sites from outside the US? Anything of
  that shape must be enumerated before Phase 2 or it will be blocked with no
  allowlist available to except it (see OQ-B).
- **OQ-G (DevOps).** Alerting on the ERROR heartbeat. The `logging` role's
  Vector→Loki path exists on app01; §2.4's argument against fail-open depends on
  the unusable-blocklist state being *noticed*, and under fail-closed it is
  noticed within minutes by 16 sites going down — but a `ready-then-empty` state
  and a stale-cache-fallback (Fix B) both want a real alert. Recommend a Loki
  alert on `check: blocklist still unusable` before Phase 2.
- **OQ-H (Jeff).** Scope: are `umami` (`analytics.bitsalt.com`) and
  `portfolio-service` in the filtered set at all? Both carry machine traffic
  (HTTP-01 ACME validation, GitHub webhooks). Architect lean: **exclude both**;
  the benefit is negligible and the failure modes are silent.

---

## Cross-references

- `docs/interfaces/traefik-middleware-attachment.md` — the DevOps-facing
  contract derived from §1, §4.3, and §4.4.
- `docs/interfaces/cache-freshness-and-stale-fallback.md` — the Developer-facing
  spec for §2.5's Fix A and Fix B.
- `docs/adr/0004-fail-open-wiring-and-readiness-observability.md` §A, §C, §E —
  the posture this ADR preserves; §E.2 carries a correction block filed by this
  pass.
- `docs/adr/0002-disk-cache-snapshot-format.md` §C and § Alternatives ("No
  staleness bound") — the guardrail §2.3 finds inoperative.
- `docs/adr/0003-geoip-source-db-ip-over-maxmind.md` — the monthly publication
  cadence that drives the 304-vs-mtime interaction.
- `docs/deployment.md` § Client-IP trust, § Run mode (a) — the cache-volume and
  never-publicly-reachable requirements this ADR inherits unchanged.
- `docs/traefik-integration.md` § Request contract, § Response contract.
- `internal/fetcher/fetcher.go:139-170, 216-221` — the 304 short-circuit and the
  conditional-GET validators.
- `internal/diskcache/cache.go:99-120` — mtime-based staleness.
- `cmd/bitblocker/main.go:190-221` — `loadDiskCache` and `removeUnusableCache`.
- `internal/scheduler/scheduler.go:19-23, 164-194` — the cold-start retry budget.
- `internal/server/server.go:216-278` — `handleCheck`; the absence of any
  `X-Forwarded-Host` read.
- `bitsalt/ansible`: `site.yml:89-141` (fleet composition),
  `roles/traefik/templates/docker-compose.yml.j2:40,44,46,50,65-68`,
  `roles/wordpress/templates/docker-compose.yml.j2:118-157`,
  `roles/wp_container_autoheal/templates/autoheal.sh.j2:41-53`.
- Coding standards §14 (additive extension over redefinition — §3.2's
  `policies:` block), §4 (explicit boundaries), §15 (YAGNI/KISS — §3).

---

*End of ADR 0005.*
