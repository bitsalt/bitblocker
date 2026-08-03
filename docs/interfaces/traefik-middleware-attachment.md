# Interface: Traefik middleware attachment on a shared multi-tenant host

> **Boundary owner:** the deployment's Traefik configuration (`bitsalt/ansible`,
> `roles/traefik` + the per-site roles). BitBlocker's own code is unchanged by
> anything in this document.
> **Consumers:** DevOps (the ansible role), the operator (the incident runbook),
> and any future role adding a site or a router to app01.
> **Governing standards:** `coding-standards.md` §14 (interface design), §4
> (explicit boundaries).
> **Decisions:** ADR 0005 (this contract), ADR 0004 (`/healthz` semantics — §7
> depends on them), ADR 0002 (the cache the volume in §2.3 must persist).

This spec is the contract DevOps builds the ansible role against. It states
**what must be true**, not how to render it — Jinja shape, variable naming
beyond the two names fixed in §3 and §4.1, task ordering, and handler wiring are
DevOps's. Where something is DevOps's call it is marked **[DevOps's call]**.

Do not re-derive the reasoning; it is in ADR 0005.

---

## 1. Purpose of the boundary

One BitBlocker daemon carries one country policy (`block.countries` is a single
flat global list, `internal/config/config.go:72-75`). app01 runs ~18 sites behind
one shared Traefik v3.6.13, and two of them must be exempt from country
filtering. `/check` decides from the client IP alone and never reads
`X-Forwarded-Host`, so **every per-site policy decision is made in Traefik, by
choosing which routers call `/check` at all.**

This boundary therefore has to carry four things the daemon cannot:

1. Which sites are filtered (§3).
2. Which *routers* of a filtered site are covered, and which must never be (§5).
3. How enforcement is switched off fleet-wide without touching any site (§4.2).
4. How the daemon's failure is prevented from becoming the host's failure (§6, §7).

---

## 2. Daemon placement contract

### 2.1 Reachability — the hard requirement

**BitBlocker's `/check` must be reachable by Traefik and by nothing else.**

If `/check` is reachable from off-host, an attacker sets `X-Real-IP` to any
address and receives a 200, bypassing the blocklist entirely with no trace at
the edge (`docs/deployment.md` § Client-IP trust). This is not a hardening
nicety; it is the precondition that makes the control mean anything.

Satisfy it by attaching the container to the existing `proxy` Docker network
with **no published port** and `listen.host: 0.0.0.0` in its config, **or** by
binding `listen.host: 127.0.0.1` under systemd on the host. Do not publish
`8080` on any interface, including `0.0.0.0:8080` behind UFW. **[DevOps's call]**
which of the two run modes; both are supported by shipped artifacts
(`docs/deployment.md` § Run mode (a) / (b)).

### 2.2 Image pinning

Pin a specific tag — `ghcr.io/bitsalt/bitblocker:1.0.0` or `:1.0`. **Never
`:latest`** (`docs/deployment.md` § Image). A `:latest` deploy moves under a
running fleet on the next release.

### 2.3 Cache persistence — required, not optional

`cache.path` (default `/var/cache/bitblocker/dbip-country-lite.mmdb`) **must sit
on storage that survives container recreation** — a named volume or a host bind
mount. The `Dockerfile` declares a `VOLUME`, so an anonymous volume is created
even without `-v`, and an anonymous volume **does not survive `docker rm`**.

Losing the cache on every recreate means every ansible apply that recreates the
container cold-starts the daemon fail-closed with no disk fallback. Under
container mode this is the single most consequential line in the compose file.
Under systemd mode, `CacheDirectory=bitblocker` in the shipped unit already
provides it — install the unit as-is rather than re-deriving its directives.

### 2.4 Config file

No config ships in the image; the daemon fails to start without one at
`--config` (default `/etc/bitblocker/config.yaml`). Values fixed by ADR 0005:

| Field | Value | Why |
|---|---|---|
| `behavior.startup_mode` | `fail-closed` | ADR 0005 §2.6. Do not set `fail-open`. |
| `behavior.log_blocked` | `true` (the default) | Required by the §8 V3 verification. |
| `cache.max_age` | `48h` (the default) | Do not raise it to work around the mtime defect (ADR 0005 § Alternatives). |
| `block.countries` | **operator input** | ADR 0005 OQ-A. Not DevOps's, not Architect's. |

---

## 3. The per-site flag

Each site dict under `bitsalt/ansible` `vars/sites/<site>.yml` carries **one
required key**:

```yaml
<name>_site:
  enabled: true
  site_name: <name>
  geo_filter: enforce        # required — "enforce" | "exempt"
```

### 3.1 Contract

| Property | Value |
|---|---|
| Key name | `geo_filter` |
| Location | The site dict in `vars/sites/<site>.yml`, alongside `enabled` |
| Type | String, one of exactly `"enforce"` or `"exempt"` |
| Default | **None. There is no default.** |
| Omitted | **Fails the play**, with a message naming the site and the two legal values |
| Invalid value | **Fails the play**, same message |

The assert runs **before** any template is rendered or any container is
recreated, so a missing flag costs an apply-time error rather than a live
policy change. **[DevOps's call]** whether that is a `pre_tasks` assert in
`site.yml` or an assert at the top of each site role; the requirement is only
that it fires before the first change.

### 3.2 Why no default

Stated so it is not "simplified" later: a new site must neither silently acquire
a geo policy nor silently lose one, and every possible default violates one of
those. Requiring the key is the only resolution — the sole outcome of omission
is a loud failure before anything changes. A string enum rather than a boolean
so that `grep -c 'geo_filter: exempt' vars/sites/*.yml` is an auditable count,
and so YAML's coercion of `no`/`off`/`"false"` cannot silently invert the
control.

### 3.3 Applies to all three site families

`geo_filter` is required on WordPress, Node.js, and FastAPI site dicts alike.
Node.js and FastAPI routers emit **no `middlewares=` label today** — that
plumbing does not exist and is Phase-3 work (ADR 0005 §4.1). Until it exists,
those sites carry `geo_filter: exempt`, and the flag being present-and-exempt is
a deliberate statement rather than an omission.

---

## 4. The middleware definition

### 4.1 One definition, in the file provider

The `forwardAuth` middleware is defined **exactly once**, in Traefik's file
provider directory (`{{ traefik_proxy_dir }}/dynamic/`, rendered by
`roles/traefik`), under the name **`bitblocker`**, and referenced from routers
as **`bitblocker@file`** (the `@file` suffix is required when a docker-provider
router references a file-provider middleware — same as the existing
`hsts-headers@file`).

It is **not** defined via docker labels on the daemon's container. Doing so
would put the §4.2 kill switch behind a container recreate and couple the
middleware's existence to the lifecycle of the very thing being toggled.

Enforcing form:

```yaml
http:
  middlewares:
    bitblocker:
      forwardAuth:
        address: "http://<daemon-host>:8080/check"
        trustForwardHeader: true
```

`trustForwardHeader: true` is **required**, not cosmetic — it is what causes
Traefik to present the `X-Forwarded-For` / `X-Real-IP` it derived from the real
TCP peer (`docs/traefik-integration.md` § Register the middleware). Without it
the daemon has no client address to decide on and fails closed on every request.

A role variable — suggested `bitblocker_address` **[DevOps's call on the name]**
— carries the address so it is stated once.

### 4.2 The kill switch — and the trap in it

A second role variable, suggested **`bitblocker_enforcing`** (bool, default
`true`), selects between two renderings of the *same* middleware name.

Disabled form:

```yaml
http:
  middlewares:
    bitblocker:
      headers:
        customRequestHeaders:
          X-BitBlocker-Enforcement: "disabled"
```

**The trap, stated as a hard constraint:**

> **The middleware named `bitblocker` must be defined in every rendering.** A
> Traefik router that references a middleware which does not exist is placed in
> an error state and stops serving its rule. "Disable enforcement by removing
> the middleware definition" would therefore take down every attached site —
> the exact opposite of a kill switch.

The disabled form is a benign pass-through, never an absent definition, never an
empty `chain`. The added request header is inert to WordPress, Node, and
FastAPI, and doubles as positive evidence in backend logs that enforcement is
off.

**Verify before relying on it** (Traefik v3.6.13, on app01, at Phase 0): flip
`bitblocker_enforcing` to `false`, confirm Traefik's file watcher picks up the
change with **no container restarted**, and confirm the attached pilot site
still serves 200. The whole value of this lever is that it is fast and
restart-free; that property must be observed, not assumed.

### 4.3 What this lever is and is not

It is a **fleet-wide enforcement toggle**. It is not a per-site control (that is
§3) and it is not a way to remove BitBlocker (that is a full apply). It is the
lever the incident runbook reaches for first.

---

## 5. Router attachment

### 5.1 The rule

> **Every router whose rule can terminate at a site's own backend carries
> `bitblocker@file` when that site is `geo_filter: enforce`. Redirect-only
> routers may omit it. Routers serving machine traffic never carry it.**

The rule is stated in terms of router *behavior* rather than router *names* so
it survives routers added after this document.

### 5.2 WordPress routers — the enumeration as of 2026-08-03

Verified against `roles/wordpress/templates/docker-compose.yml.j2`:

| Router | Rule | Carries `bitblocker@file`? |
|---|---|---|
| `{site}` (apex, line 118) | `Host(domain)` | **Yes** |
| `{site}-login` (line 135) | `Host(domain) && Path(/wp-login.php)` | **Yes — do not miss this one** |
| `{site}-block-batch` (line 154) | `Host(domain) && POST && batch paths` | No — already 403s everything via a TEST-NET `ipAllowList` |
| `{site}-www` (line 185) | `Host(www.domain)` | No — `redirectregex` only; the client is blocked one hop later at the apex |
| `{site}-alias-N`, `-alias-N-www` | alias hosts | No — same reason |

**`{site}-login` is the attachment that must not be missed.** Its rule is longer
than the apex router's, so it wins match priority; attaching only to the apex
leaves the WordPress login form — the single highest-value target on these sites
— reachable from every blocked country. This is not a hypothetical failure mode
in this repo: the comments at lines 122-138 and 140-154 document two controls
that were inert in production for months because a per-router attribute was
missing on exactly these two routers.

### 5.3 Chain position

`bitblocker@file` goes **first** in a router's comma-separated middleware list:

```
traefik.http.routers.{{ site }}.middlewares=bitblocker@file,{{ site }}-block-xmlrpc
```

Traefik applies the chain in order. Placing the geo decision first means a
blocked request is rejected before any rewrite or rate-limit work is done, and
means the decision is never taken against a path some earlier middleware
rewrote.

### 5.4 Routers that must NEVER carry it

| Surface | Why |
|---|---|
| The `web` entrypoint / ACME HTTP-01 challenge path | The `le-http` resolver validates over port 80 from Let's Encrypt's vantage points, which are multi-regional. Filtering that path breaks certificate renewal — silently, ~60 days before anyone notices. DNS-01 (`le`) is unaffected. |
| `portfolio-service` `/internal/*` (GitHub + Asana webhooks) | Already constrained by `internal-ip-allowlist`. A second, geo-shaped failure mode on a push-triggered sync buys nothing. |
| Any external uptime monitor or synthetic-check path | Must be enumerated before Phase 2 (ADR 0005 OQ-F). There is no per-IP allowlist in v1 (OQ-3, still open), so a blocked monitor cannot be excepted except by unattaching the whole site. |

### 5.5 Never at the entrypoint

**Do not add `bitblocker@file` to
`--entrypoints.websecure.http.middlewares`.** Traefik's `forwardAuth` to an
unreachable address returns **500 for the original request**, so any BitBlocker
process failure — stopped, crashed, OOM-killed, mid-pull, wedged — would take
down every router on the entrypoint, including both exempt clients,
`portfolio-service`, and umami. Per-router attachment is what makes the
exemption structural rather than dependent on the exempting process being alive.
Full reasoning: ADR 0005 §1.4.

---

## 6. Failure semantics — what each state does to each site

| BitBlocker state | `geo_filter: enforce` site | `geo_filter: exempt` site | Non-attached router (ACME, `/internal/`) |
|---|---|---|---|
| Healthy, blocklist usable | Enforcing: 403 for blocked countries, 200 otherwise | **Unaffected** | **Unaffected** |
| Running, blocklist UNUSABLE (`fail-closed`) | **403 for everyone** | **Unaffected** | **Unaffected** |
| Process down / unreachable | **Traefik 500 for everyone** | **Unaffected** | **Unaffected** |
| `bitblocker_enforcing: false` | 200, unfiltered, header stamped | Unaffected | Unaffected |

The two right-hand columns are the point of this design. Read them before
proposing entrypoint attachment.

---

## 7. Health checking — a hard constraint

> **BitBlocker's container declares no `HEALTHCHECK`.**

`roles/wp_container_autoheal` installs a host-side timer running
`docker ps --filter health=unhealthy` and restarts **any** container on app01
reporting unhealthy (`templates/autoheal.sh.j2:41-53`). It is fleet-scoped, not
WordPress-scoped.

A `HEALTHCHECK` wired to `/healthz` composes with that into a harmful loop:
unusable blocklist ⇒ `/healthz` 503 (deliberately — ADR 0004 §C) ⇒ container
unhealthy ⇒ autoheal restarts ⇒ the restart re-runs `loadDiskCache`, which
**deletes** a stale cache and resets the cold-start retry budget ⇒ still
unusable ⇒ restart again at the cooldown interval. Every iteration destroys
recovery state and gains nothing: **restarting a daemon that has no blocklist
does not give it a blocklist.**

`/healthz` is a **readiness** signal, not a liveness signal. Monitor it
externally instead:

- Poll `/healthz` from the host (`200` + `"serving":"enforcing"` + `prefixes > 0`
  is the healthy shape) and alert; **do not restart on it.**
- Alert on the daemon's mandatory ERROR heartbeat via the existing Vector→Loki
  path: log message `check: blocklist still unusable`, emitted every 60s while
  the state persists, not suppressible by any config (ADR 0004 §D.2).

The shipped image is distroless with no shell or fetch tool, so the naive
`HEALTHCHECK` does not work anyway. This constraint exists so a well-intentioned
wrapper image does not reintroduce it.

**[DevOps's call]:** whether the external `/healthz` poll is a systemd timer, a
Vector check, or a Loki-side alert on the heartbeat alone.

---

## 8. Verification obligations

Detail and exact commands: ADR 0005 §4.2. Summarized here because these are
gate conditions on the rollout, not optional extras.

| # | When | Check | Gates |
|---|---|---|---|
| **V1** | Phase 0 | `/check` returns the block status for a blocked-country IP and 200 for a US IP, via direct `curl` on the host. `/healthz` is 200 with `prefixes > 0`. Restart the daemon and confirm `/healthz` returns to 200 with no visible window. | Phase 1 |
| **V2** | Phase 1 | End-to-end through Traefik on :443 from a real egress, using a temporary **canary country** added to `block.countries`, then removed. Proves the whole chain, not just the daemon. | Phase 2 |
| **V3** | Phase 1 | **The inert-control check.** Confirm the redacted client addresses in the daemon's logs **vary across requests** rather than being one constant `172.21.x.x`. | Phase 2 |
| **V4** | Phase 0 | Flip `bitblocker_enforcing` to `false` and back; confirm Traefik picks it up with no container restarted and the site keeps serving. | Phase 1 |

**V3 is the one most likely to be skipped and the one with precedent.**
BitBlocker takes the **rightmost** `X-Forwarded-For` entry. If Traefik's
`forwardAuth` call presents XFF as `<client>, <traefik-internal-IP>` rather than
a bare `<client>`, the rightmost entry is Traefik's own address — in no blocked
country — and the daemon allows everything while looking perfectly healthy:
`/healthz` 200, populated trie, no errors, no signal anywhere. The control would
be entirely decorative and V1 would not reveal it.

The header contract is *expected* to be clean on app01: Traefik scopes
`forwardedHeaders.trustedIPs` to the docker-proxy subnet
(`roles/traefik/templates/docker-compose.yml.j2:44,46`), so an untrusted public
peer's own XFF is discarded and rewritten from the real TCP peer, and
`ansible/lessons/umami-geo-granularity-client-ip-header.md` confirms nothing
fronts app01. **That is a reason to expect V3 to pass, not a reason to skip it.**

---

## 9. Rollback levers — ordered, and the anti-lever

1. **Fleet-wide, ~10 seconds, no restarts:** `bitblocker_enforcing: false`,
   render the file-provider fragment in its disabled form (§4.2). No container
   restarted, no router label changed, no site role re-applied. **This is the
   incident lever.**
2. **Per-site, minutes:** `geo_filter: exempt` on one site, re-apply that site's
   role.
3. **Full removal, one apply:** all sites to `exempt`, then remove the daemon.

**The anti-lever — put this in the runbook in these words:**

> **Do not stop, kill, or `docker rm` the BitBlocker container to "turn it
> off."** A `forwardAuth` middleware pointing at a dead address makes Traefik
> return **500 for every attached site**. Stopping the daemon converts a partial
> problem into a total outage of everything it is attached to. Use lever 1.

Lever 1 exists specifically so this is never the reachable option under
incident pressure, where the instinct — stop the thing causing trouble — is
exactly wrong.

---

## 10. Versioning stance

- **`/check`'s status-code semantics are the `forwardAuth` contract** and are
  unchanged by anything here. Nothing in this document alters BitBlocker's code.
- **`geo_filter` is a new required key on an internal ansible data structure.**
  Adding it is a breaking change to every site vars file: all 18 must carry it
  before the next full apply. That is deliberate (§3.2) and means the ansible
  change cannot land piecemeal.
- **`bitblocker@file` is a name other roles will reference.** Renaming it later
  breaks every router label that names it and, per §4.2's trap, takes those
  routers out of service rather than merely unfiltering them. Treat the name as
  a published contract.
- **§5.2's router enumeration is a point-in-time snapshot.** §5.1's rule is the
  durable contract; the table is the current application of it. A router added
  to a site role after this date must be evaluated against §5.1 — that
  evaluation is the mitigation for the drift risk ADR 0005 § Consequences
  accepts.

---

## 11. Cross-references

- `docs/adr/0005-shared-host-per-site-policy-attachment-and-fail-mode.md` — the
  decision and its reasoning. Read it before changing anything here.
- `docs/adr/0004-fail-open-wiring-and-readiness-observability.md` §C, §D —
  `/healthz` readiness semantics and the mandatory ERROR heartbeat that §7
  depends on.
- `docs/interfaces/cache-freshness-and-stale-fallback.md` — the Developer-side
  changes (Fix A / Fix B) that bound the fail-closed window; Fix A should land
  before Phase 2.
- `docs/deployment.md` § Image, § Run mode (a)/(b), § Configuration essentials,
  § Client-IP trust.
- `docs/traefik-integration.md` § Register the middleware, § Request contract,
  § Response contract, § `/healthz` as a health check.
- `bitsalt/ansible`: `site.yml:89-141`,
  `roles/traefik/templates/docker-compose.yml.j2:40,44,46,50,65-68`,
  `roles/traefik/templates/dynamic.yml.j2`,
  `roles/wordpress/templates/docker-compose.yml.j2:118-157`,
  `roles/wp_container_autoheal/templates/autoheal.sh.j2:41-53`.
- Coding standards §14, §4.
